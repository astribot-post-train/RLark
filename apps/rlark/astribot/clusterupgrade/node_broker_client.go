package clusterupgrade

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

// BrokerNodeImages binds the executor adapter to one approved image target and
// one Pod identity. The token source is read on each call to allow rotation.
type BrokerNodeImages struct {
	endpoint   string
	cluster    string
	identity   ExecutorIdentity
	target     string
	deployment Images
	token      func() (string, error)
	client     *http.Client
}

func brokerURL(origin string) (*url.URL, error) {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.Path != "" || parsed.Fragment != "" || parsed.RawQuery != "" || parsed.ForceQuery {
		return nil, errors.New("a valid HTTPS origin is required for the node image broker")
	}
	return parsed, nil
}

func brokerTLSClient(origin, caPEM string) (*http.Client, error) {
	if _, err := brokerURL(origin); err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, errors.New("a trusted CA is required for the node image broker")
	}
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
	}}, nil
}

func NewBrokerNodeImages(origin, cluster, target string, deployment Images, identity ExecutorIdentity, token func() (string, error), client *http.Client) (*BrokerNodeImages, error) {
	parsed, err := brokerURL(origin)
	if err != nil || token == nil || client == nil {
		return nil, errors.New("an HTTPS node image broker origin and token source are required")
	}
	if _, err := pinnedImage(target); err != nil {
		return nil, err
	}
	parsed.Path = NodeImageBrokerPath
	transport := *client
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if transport.Timeout == 0 {
		transport.Timeout = 15 * time.Second
	}
	return &BrokerNodeImages{endpoint: parsed.String(), cluster: cluster, identity: identity, target: target, deployment: deployment, token: token, client: &transport}, nil
}

var _ NodeAgentImages = (*BrokerNodeImages)(nil)

func (a *BrokerNodeImages) Read(ctx context.Context) (NodeAgentTarget, error) {
	state, err := a.call(ctx, ReadNodeImage)
	if err != nil {
		return NodeAgentTarget{}, err
	}
	if state.Phase != NodeImageApproved {
		return NodeAgentTarget{}, ErrNodeImageGrant
	}
	return NodeAgentTarget{ClusterUID: string(state.ClusterUID), Before: state.Before}, nil
}

func (a *BrokerNodeImages) CompareAndSet(ctx context.Context, snap NodeAgentTarget, from, to string, rollback bool) error {
	var action NodeImageAction
	// The deployment can be rolled back before the node declaration was
	// applied. Even then, explicitly spend the broker grant on rollback.
	switch {
	case !rollback && from == snap.Before && to == a.target:
		action = ApplyNodeImage
	case rollback && from == a.target && to == snap.Before:
		action = RestoreNodeImage
	default:
		return ErrNodeImageGrant
	}
	// Check the current grant before asking the broker to spend it.
	state, err := a.call(ctx, ReadNodeImage)
	if err != nil {
		return err
	}
	if !matchesNodeSnapshot(state, snap) {
		return ErrNodeImageGrant
	}
	_, err = a.call(ctx, action)
	return err
}

func (a *BrokerNodeImages) WaitReady(ctx context.Context, snap NodeAgentTarget, image string) error {
	return wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		state, err := a.call(ctx, ObserveNodeRollout)
		if err != nil {
			return false, err
		}
		if !matchesNodeSnapshot(state, snap) {
			return false, ErrNodeImageGrant
		}
		atTarget := state.Phase == NodeImageApplied && image == a.target
		restored := state.Phase == NodeImageRolledBack && image == snap.Before
		return state.Ready && (atTarget || restored), nil
	})
}

func matchesNodeSnapshot(state NodeImageResult, snap NodeAgentTarget) bool {
	return string(state.ClusterUID) == snap.ClusterUID && state.Before == snap.Before
}

func (a *BrokerNodeImages) call(ctx context.Context, action NodeImageAction) (NodeImageResult, error) {
	token, err := a.token()
	if err != nil {
		return NodeImageResult{}, err
	}
	payload, err := json.Marshal(nodeImageRequest{Cluster: a.cluster, Executor: a.identity, Action: action})
	if err != nil {
		return NodeImageResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(payload))
	if err != nil {
		return NodeImageResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := a.client.Do(req)
	if err != nil {
		return NodeImageResult{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		switch response.StatusCode {
		case http.StatusUnauthorized:
			return NodeImageResult{}, ErrUnverifiedExecutor
		case http.StatusForbidden:
			return NodeImageResult{}, ErrNodeImageGrant
		case http.StatusConflict:
			return NodeImageResult{}, ErrTargetChanged
		default:
			return NodeImageResult{}, fmt.Errorf("node image broker returned HTTP %d", response.StatusCode)
		}
	}
	var result NodeImageResult
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&result); err != nil {
		return NodeImageResult{}, err
	}
	if result.ClusterUID == "" || result.Before == "" || result.Target != a.target || result.Deployment != a.deployment {
		return NodeImageResult{}, ErrNodeImageGrant
	}
	return result, nil
}
