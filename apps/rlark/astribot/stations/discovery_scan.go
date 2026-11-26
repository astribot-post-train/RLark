package stations

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	"github.com/rlinf/rlark/apps/rlark/astribot/stationidentity"
	"github.com/rlinf/rlark/apps/rlark/pkg/log"
)

// DiscoveryScanRoutes supplies the authorized management API and HTTP integration.
type DiscoveryScanRoutes struct {
	StationClient             dynamic.Interface
	Enrollments, SiteNetworks schema.GroupVersionResource
	SiteNetwork               func(context.Context) (*unstructured.Unstructured, error)
	DecodeRequest             func(*gin.Context, any) bool
	Actor                     func(*gin.Context) string
	RespondError              func(*gin.Context, int, ErrorKind, string)
	RespondKubeError          func(*gin.Context, string, error)
}

// DiscoveryScanView reports the progress of one site scan over its original Hub set.
type DiscoveryScanView struct {
	ID        string    `json:"id"`
	StartedAt time.Time `json:"startedAt"`
	Phase     string    `json:"phase"`
	Total     int       `json:"total"`
	Completed int       `json:"completed"`
	Failed    int       `json:"failed"`
}

func discoveryScanStatus(scan stationidentity.DiscoveryScan, hubs []unstructured.Unstructured, now time.Time) DiscoveryScanView {
	result := DiscoveryScanView{ID: scan.ID, StartedAt: scan.StartedAt, Phase: "Running", Total: len(scan.Hubs)}
	targets := make(map[types.UID]bool, len(scan.Hubs))
	for _, uid := range scan.Hubs {
		targets[uid] = true
	}
	for i := range hubs {
		hub := &hubs[i]
		if targets[hub.GetUID()] {
			done, failed := stationidentity.DiscoveryScanHubResult(hub, scan.ID)
			if done {
				result.Completed++
			}
			if failed {
				result.Failed++
			}
		}
	}
	if result.Completed == result.Total {
		result.Phase = "Succeeded"
		if result.Failed > 0 {
			result.Phase = "Failed"
		}
	} else if !now.Before(scan.StartedAt.Add(stationidentity.DiscoveryScanLifetime)) {
		result.Phase = "TimedOut"
	}
	return result
}

// Handle reads or starts the scan for the requested site.
func (r DiscoveryScanRoutes) Handle(c *gin.Context) {
	site := c.Param("site")
	if len(validation.IsDNS1123Label(site)) != 0 {
		r.RespondError(c, http.StatusBadRequest, InvalidRequest, "site must be a DNS label")
		return
	}
	start := c.Request.Method == http.MethodPost
	var request struct {
		ID string `json:"id"`
	}
	if start {
		if !r.DecodeRequest(c, &request) {
			return
		}
		if id, err := uuid.Parse(request.ID); err != nil || id.Version() != 4 || id.String() != request.ID {
			r.RespondError(c, http.StatusBadRequest, InvalidRequest, "id must be a canonical version-4 UUID")
			return
		}
	}
	ctx := c.Request.Context()
	var result *DiscoveryScanView
	var rejected string
	var created bool
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		network, err := r.SiteNetwork(ctx)
		if err != nil {
			return err
		}
		sites, _, _ := unstructured.NestedSlice(network.Object, "spec", "sites")
		found := false
		for _, raw := range sites {
			if entry, ok := raw.(map[string]any); ok && entry["name"] == site {
				found = true
				break
			}
		}
		if !found {
			rejected = "site not found"
			return nil
		}
		scan, err := stationidentity.DiscoveryScanFor(network, site)
		if err != nil || scan.ID == "" && !start {
			return err
		}
		hubs, err := r.StationClient.Resource(r.Enrollments).List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if scan.ID != "" {
			current := discoveryScanStatus(scan, hubs.Items, now)
			if !start || current.Phase == "Running" || scan.ID == request.ID {
				result = &current
				return nil
			}
		}
		if discovery := stationidentity.SiteNetworkDiscovery(network, site); discovery == nil || !discovery.Enabled {
			rejected = "save discovery settings for this site before scanning"
			return nil
		}
		observed, _, _ := unstructured.NestedInt64(network.Object, "status", "observedGeneration")
		if observed < network.GetGeneration() {
			rejected = "discovery settings are still being applied"
			return nil
		}
		scan = stationidentity.DiscoveryScan{ID: request.ID, StartedAt: now}
		for i := range hubs.Items {
			hub := &hubs.Items[i]
			config, _, _ := unstructured.NestedString(hub.Object, "status", "config")
			var document struct {
				Discovery json.RawMessage `json:"discovery"`
			}
			if stationidentity.DiscoveryScanHubSite(hub) == site && json.Unmarshal([]byte(config), &document) == nil &&
				len(document.Discovery) > 0 && string(document.Discovery) != "null" {
				scan.Hubs = append(scan.Hubs, hub.GetUID())
			}
		}
		if len(scan.Hubs) == 0 {
			rejected = "no enrolled Hub has received discovery settings for this site"
			return nil
		}
		if err := stationidentity.StoreDiscoveryScan(network, site, scan); err != nil {
			return err
		}
		if _, err := r.StationClient.Resource(r.SiteNetworks).Namespace(network.GetNamespace()).Update(ctx, network, metav1.UpdateOptions{}); err != nil {
			return err
		}
		created = true
		current := discoveryScanStatus(scan, hubs.Items, now)
		result = &current
		return nil
	})
	if err != nil {
		r.RespondKubeError(c, "Site discovery scan request failed", err)
		return
	}
	if rejected == "site not found" {
		r.RespondError(c, http.StatusNotFound, NotFound, rejected)
		return
	}
	if rejected != "" {
		r.RespondError(c, http.StatusConflict, Conflict, rejected)
		return
	}
	if created {
		log.FromContext(ctx).WithName("audit").Info("site discovery scan started", "actor", r.Actor(c), "site", site, "scan", result.ID)
	}
	status := http.StatusOK
	if start && result != nil && result.Phase == "Running" {
		status = http.StatusAccepted
	}
	c.JSON(status, result)
}
