package gateway

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rlinf/rlark/apps/rlark/astribot/registry"
)

const (
	registryDetailsConcurrency = 4
	registryImageCacheEntries  = 4096
)

func (g *Gateway) handleRegistryTagDetails(c *gin.Context) {
	credential, ok := g.registryCredential(c)
	if !ok {
		return
	}
	repository, last := c.Query("repository"), c.Query("last")
	if !validRegistryRepository(repository) || (last != "" && !registryTagPattern.MatchString(last)) {
		respondError(c, http.StatusBadRequest, CodeInvalidQueryParameter, "invalid repository or tag cursor")
		return
	}
	browser := g.registryBrowser()
	tags, next, ok := listRegistryTags(c, browser, credential, repository, last)
	if !ok {
		return
	}
	details := make([]registry.TagDetail, len(tags))
	work := make(chan int)
	var wg sync.WaitGroup
	for range min(registryDetailsConcurrency, len(tags)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				details[i] = g.describeRegistryTag(c.Request.Context(), browser, credential, repository, tags[i])
			}
		}()
	}
	for i := range tags {
		if c.Request.Context().Err() != nil {
			break
		}
		work <- i
	}
	close(work)
	wg.Wait()
	if err := c.Request.Context().Err(); err != nil {
		respondError(c, http.StatusBadGateway, CodeUpstreamUnavailable, "registry request canceled")
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": details, "next": next})
}

func (g *Gateway) describeRegistryTag(ctx context.Context, browser *registryBrowser, credential storedImageRegistryCredential, repository, tag string) registry.TagDetail {
	detail := registry.TagDetail{Tag: tag}
	read := func(ctx context.Context, path string) (*http.Response, error) {
		return browser.request(ctx, credential, http.MethodGet, path, nil)
	}
	body, mediaType, digest, err := registry.FetchManifest(ctx, read, repository, tag, "")
	if err != nil {
		detail.Error = "unavailable"
		return detail
	}
	detail.Digest = digest
	key := credential.Registry + "/" + repository + "@" + digest
	cached, found := g.registryImages.Get(key)
	info, _ := cached.(registry.ImageInfo)
	if !found {
		info, err = registry.ResolveImageInfo(ctx, read, repository, body, mediaType)
		if err != nil {
			detail.Error = "unavailable"
			return detail
		}
		g.registryImages.Add(key, info)
	}
	if info.Created != nil {
		detail.Created = info.Created.UTC().Format(time.RFC3339)
	}
	detail.Size = info.Size
	detail.Platforms = info.Platforms
	return detail
}
