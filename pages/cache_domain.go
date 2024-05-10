package pages

import (
	"bufio"
	"code.gitea.io/sdk/gitea"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"github.com/allegro/bigcache/v3"
	"github.com/patrickmn/go-cache"
	"github.com/pkg/errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type DomainCache struct {
	ttl time.Duration
	*cache.Cache
	mutexes sync.Map
}

func (c *DomainCache) Close() error {
	if c.Cache != nil {
		c.Cache.Flush()
	}
	return nil
}

type DomainConfig struct {
	FetchTime int64 //上次刷新时间

	PageDomain PageDomain
	Exists     bool               // 当前项目是否为 Pages
	FileCache  *bigcache.BigCache // 文件缓存

	CNAME    []string // 重定向地址
	SHA      string   // 缓存 SHA
	BasePath string   // 根目录
	Topics   []string // 存储库标记

	Index    string //默认页面
	NotFound string //不存在页面
}

func (receiver *DomainConfig) Close() error {
	return receiver.FileCache.Close()
}

func NewDomainCache(ttl time.Duration) DomainCache {
	c := cache.New(5*time.Minute, 10*ttl)
	c.OnEvicted(func(_ string, i interface{}) {
		config := i.(*DomainConfig)
		if config != nil {
			err := config.Close()
			if err != nil {
				return
			}
		}
	})
	return DomainCache{
		ttl:     ttl,
		Cache:   c,
		mutexes: sync.Map{},
	}
}

func fetch(client *GiteaConfig, domain *PageDomain, result *DomainConfig) error {
	branches, resp, err := client.Client.ListRepoBranches(domain.Owner, domain.Repo,
		gitea.ListRepoBranchesOptions{
			ListOptions: gitea.ListOptions{
				PageSize: 999,
			},
		})
	// 缓存 404 内容
	if resp != nil && resp.StatusCode >= 400 && resp.StatusCode < 500 {
		result.Exists = false
		return nil
	}
	if err != nil {
		return err
	}
	topics, resp, err := client.Client.ListRepoTopics(domain.Owner, domain.Repo, gitea.ListRepoTopicsOptions{
		ListOptions: gitea.ListOptions{
			PageSize: 999,
		},
	})
	if err != nil {
		return err
	}
	branchIndex := slices.IndexFunc(branches, func(x *gitea.Branch) bool { return x.Name == domain.Branch })
	if branchIndex == -1 {
		return errors.Wrap(ErrorNotFound, "branch not found")
	}
	currentSHA := branches[branchIndex].Commit.ID
	result.Topics = topics
	if result.SHA == currentSHA {
		// 历史缓存一致，跳过
		result.FetchTime = time.Now().UnixMilli()
		return nil
	}
	// 清理历史缓存
	if result.SHA != currentSHA {
		if result.FileCache != nil {
			err := result.FileCache.Reset()
			if err != nil {
				return err
			}
		}
		result.SHA = currentSHA
	}
	//查询是否为仓库
	result.Exists, err = client.FileExists(domain, "/index.html")
	if err != nil {
		return err
	}
	if !result.Exists {
		return nil
	}
	result.Index = "index.html"
	//############# 处理 404
	notFound, err := client.FileExists(domain, "/404.html")
	if err != nil {
		return err
	}
	if notFound {
		result.NotFound = "404.html"
	} else {
		result.NotFound = "index.html"
	}
	// ############ 拉取 CNAME
	cname, err := client.ReadStringRepoFile(domain, "/CNAME")
	if err != nil && !errors.Is(err, ErrorNotFound) {
		// ignore not fond error
		return err
	} else if cname != "" {
		// 清理重定向
		result.CNAME = make([]string, 0)
		scanner := bufio.NewScanner(strings.NewReader(cname))
		for scanner.Scan() {
			alias := scanner.Text()
			alias = strings.TrimSpace(alias)
			alias = strings.TrimPrefix(strings.TrimPrefix(alias, "https://"), "http://")
			alias = strings.Split(alias, "/")[0]
			if len(strings.TrimSpace(alias)) > 0 {
				result.CNAME = append(result.CNAME, alias)
			}
		}
	}
	result.FetchTime = time.Now().UnixMilli()
	return nil
}

type PageCacheInfo struct {
	MODE   string `json:"MODE"`
	SHA    string `json:"SHA"`
	UPDATE string `json:"UPDATE"`
}

func (receiver *PageCacheInfo) raw() string {
	marshal, _ := json.Marshal(receiver)
	return string(marshal)
}

func (receiver *DomainConfig) Copy(
	client *GiteaConfig,
	path string,
	writer http.ResponseWriter,
	_ *http.Request,
	maxSize int,
) (bool, error) {
	if strings.HasSuffix(path, "/") {
		path = path + "index.html"
	}
	cacheInfo := &PageCacheInfo{
		MODE:   "MISS",
		SHA:    receiver.SHA,
		UPDATE: time.UnixMilli(receiver.FetchTime).Format(time.RFC1123),
	}

	pathTag := fmt.Sprintf("\"%x\"",
		sha1.Sum([]byte(
			fmt.Sprintf("%s|%s|%s", receiver.SHA, receiver.PageDomain.Key(), path))))
	contentType := mime.TypeByExtension(filepath.Ext(path))
	// 开启缓存
	data, err := receiver.FileCache.Get(path)
	if err == nil {
		cacheInfo.MODE = "HIT"
		for k, v := range client.CustomHeaders {
			writer.Header().Set(k, v)
		}
		writer.Header().Set("ETag", pathTag)
		writer.Header().Set("Pages-Server-Cache", cacheInfo.raw())
		writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
		writer.Header().Add("Content-Type", contentType)
		writer.WriteHeader(http.StatusOK)
		_, err := writer.Write(data)
		return true, err
	} else if errors.Is(err, bigcache.ErrEntryNotFound) {
		cacheInfo.MODE = "MISS"
		// 使用 SHA 抓取内容
		domain := *(&receiver.PageDomain)
		domain.Branch = receiver.SHA
		ctx, err := client.OpenFileContext(&domain, receiver.BasePath+path)
		if errors.Is(err, ErrorNotFound) && receiver.NotFound != "" {
			ctx, err = client.OpenFileContext(&domain, receiver.BasePath+receiver.NotFound)
		}
		if err != nil {
			return false, err
		}
		contentLength := ctx.Header.Get("Content-Length")
		length, err := strconv.Atoi(contentLength)
		skipCache := length >= maxSize
		if maxSize <= 0 {
			skipCache = false
		}
		if skipCache {
			cacheInfo.MODE = "SKIP"
		}
		for k, v := range client.CustomHeaders {
			writer.Header().Set(k, v)
		}
		writer.Header().Set("ETag", pathTag)
		writer.Header().Set("Pages-Server-Cache", cacheInfo.raw())
		writer.Header().Set("Content-Length", contentLength)
		writer.Header().Add("Content-Type", contentType)
		writer.WriteHeader(http.StatusOK)
		if skipCache {
			// 文件过大，跳过缓存
			_, err = io.Copy(writer, ctx.Body)
			return true, err
		} else {
			all, err := io.ReadAll(ctx.Body)
			if err != nil {
				return true, err
			}
			err = receiver.FileCache.Set(path, all)
			if err != nil {
				return true, err
			}
			_, err = writer.Write(all)
			return true, err
		}
	} else {
		return false, err
	}

}

// FetchRepo 拉取 Repo 信息
func (c *DomainCache) FetchRepo(client *GiteaConfig, domain *PageDomain) (*DomainConfig, bool, error) {
	nextTime := time.Now().UnixMilli() - c.ttl.Milliseconds()
	lock := c.Lock(domain)
	defer lock()
	cacheKey := domain.Key()
	result, exists := c.Get(cacheKey)
	if !exists {
		bigCache, err := bigcache.New(context.Background(), bigcache.DefaultConfig(10*time.Minute))
		if err != nil {
			return nil, false, err
		}
		result = &DomainConfig{
			PageDomain: *domain,
			FileCache:  bigCache,
		}
		if err = fetch(client, domain, result.(*DomainConfig)); err != nil {
			return nil, false, err
		}
		err = c.Add(cacheKey, result, cache.DefaultExpiration)
		if err != nil {
			return nil, false, err
		}
		return result.(*DomainConfig), false, nil
	} else {
		config := result.(*DomainConfig)
		if nextTime > config.FetchTime {
			// 刷新旧的缓存
			if err := fetch(client, domain, config); err != nil {
				return nil, false, err
			}
		}
		return config, true, nil
	}

}

func (c *DomainCache) Lock(any *PageDomain) func() {
	return c.LockAny(any.Key())
}

func (c *DomainCache) LockAny(any string) func() {
	value, _ := c.mutexes.LoadOrStore(any, &sync.Mutex{})
	mtx := value.(*sync.Mutex)
	mtx.Lock()

	return func() { mtx.Unlock() }
}