package stationidentity

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type AgentConfig struct {
	APIURL                   string
	ClusterID                string
	TokenFile                string
	CAFile                   string
	Interval                 time.Duration
	RequestLimit             time.Duration
	ProbeAddress             string
	WireguardObservationFile string
	// NetworkIdentityFile is where the gateway container publishes its public
	// key; NetworkConfigFile is where the agent stores the computed
	// configuration for it. Both empty disables network enrollment.
	NetworkIdentityFile string
	NetworkConfigFile   string
}

func (c AgentConfig) Validate() error {
	parsed, err := url.Parse(c.APIURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
		return errors.New("Station API URL must be an HTTPS origin or path")
	}
	if c.ClusterID == "" || len(c.ClusterID) > 128 || c.TokenFile == "" {
		return errors.New("cluster ID and projected token path are required")
	}
	if c.Interval < 5*time.Second || c.Interval > 10*time.Minute || c.RequestLimit < time.Second || c.RequestLimit > time.Minute {
		return errors.New("agent interval or request timeout is outside the supported range")
	}
	if c.ProbeAddress != "" {
		_, port, err := net.SplitHostPort(c.ProbeAddress)
		n, portErr := strconv.Atoi(port)
		if err != nil || portErr != nil || n < 1 || n > 65535 {
			return errors.New("agent probe address must contain a valid TCP port")
		}
	}
	if (c.NetworkIdentityFile == "") != (c.NetworkConfigFile == "") {
		return errors.New("network identity and configuration files are set together")
	}
	return nil
}

type Agent struct {
	config               AgentConfig
	client               *http.Client
	read                 func(string) ([]byte, error)
	post                 func(context.Context, string, []byte, string) error
	now                  func() time.Time
	mu                   sync.RWMutex
	lastSuccessfulReport time.Time
	observe              func() (Observation, error)
}

func NewAgent(config AgentConfig) (*Agent, error) {
	return newAgent(config, os.ReadFile, time.Now)
}

func newAgent(config AgentConfig, read func(string) ([]byte, error), now func() time.Time) (*Agent, error) {
	if config.ProbeAddress == "" {
		config.ProbeAddress = ":8081"
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if read == nil || now == nil {
		return nil, errors.New("token reader and clock are required")
	}
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: config.RequestLimit, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   config.RequestLimit,
		ResponseHeaderTimeout: config.RequestLimit,
		IdleConnTimeout:       60 * time.Second,
		MaxIdleConns:          16,
	}
	if config.CAFile != "" {
		pem, err := read(config.CAFile)
		if err != nil {
			return nil, errors.New("read Station API CA bundle")
		}
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("Station API CA bundle contains no certificate")
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   config.RequestLimit,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("Station API redirects are not allowed")
		},
	}
	agent := &Agent{config: config, client: client, read: read, now: now, observe: collectSystemObservation}
	agent.post = agent.postObservation
	return agent, nil
}

func (a *Agent) Run(ctx context.Context) error {
	if a == nil {
		return errors.New("Station agent is required")
	}
	probeServer := &http.Server{
		Addr: a.config.ProbeAddress, Handler: a.probeHandler(),
		ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second,
		WriteTimeout: 3 * time.Second, IdleTimeout: 10 * time.Second,
	}
	listener, err := net.Listen("tcp", a.config.ProbeAddress)
	if err != nil {
		return fmt.Errorf("listen for Station agent health probes: %w", err)
	}
	probeErr := make(chan error, 1)
	go func() { probeErr <- probeServer.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = probeServer.Shutdown(shutdownCtx)
	}()
	backoff, enrollment := time.Second, ""
	maxBackoff := min(a.config.Interval, 30*time.Second)
	for {
		select {
		case err := <-probeErr:
			if !errors.Is(err, http.ErrServerClosed) {
				return errors.New("Station agent probe listener stopped")
			}
		default:
		}
		if err := a.Report(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = time.Second
		if a.config.NetworkConfigFile != "" {
			state, err := a.Enroll(ctx)
			if err != nil {
				state = err.Error()
			}
			if state != enrollment {
				enrollment = state
				fmt.Fprintln(os.Stderr, "network enrollment:", state)
			}
		}
		timer := time.NewTimer(a.config.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (a *Agent) Report(ctx context.Context) error {
	if a == nil {
		return errors.New("Station agent is required")
	}
	tokenBytes, err := a.read(a.config.TokenFile)
	if err != nil {
		return errors.New("read projected Station token")
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" || strings.ContainsAny(token, "\r\n \t") {
		return errors.New("projected Station token is invalid")
	}
	observation, err := a.observe()
	if err != nil {
		return fmt.Errorf("collect Station observation: %w", err)
	}
	if a.config.WireguardObservationFile != "" {
		observation.Wireguard = readWireguardSnapshot(a.config.WireguardObservationFile, a.now())
	}
	body, err := json.Marshal(observationRequest{ClusterID: a.config.ClusterID, Observation: observation})
	if err != nil {
		return errors.New("encode Station observation")
	}
	if err := a.post(ctx, strings.TrimRight(a.config.APIURL, "/")+"/v1/observations", body, token); err != nil {
		return err
	}
	a.mu.Lock()
	a.lastSuccessfulReport = a.now().UTC()
	a.mu.Unlock()
	return nil
}

// A missing, malformed or stale snapshot clears the previous observation while
// leaving the identity heartbeat independent of the network container.
func readWireguardSnapshot(path string, now time.Time) json.RawMessage {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 16*1024+1))
	if err != nil || len(data) > 16*1024 {
		return nil
	}
	var snapshot struct {
		ObservedAt time.Time `json:"observedAt"`
		Interface  string    `json:"interface"`
		Addresses  []string  `json:"addresses"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if dec.Decode(&snapshot) != nil {
		return nil
	}
	var extra any
	if dec.Decode(&extra) != io.EOF || snapshot.ObservedAt.IsZero() || snapshot.ObservedAt.After(now.Add(5*time.Second)) || now.Sub(snapshot.ObservedAt) > 30*time.Second {
		return nil
	}
	raw, err := json.Marshal(wireguardObservation{Interface: snapshot.Interface, Addresses: snapshot.Addresses})
	if err != nil || validateWireguardObservation(raw) != nil {
		return nil
	}
	return raw
}

func collectSystemObservation() (Observation, error) {
	data, err := os.Open("/proc/stat")
	if err != nil {
		return Observation{}, err
	}
	defer func() { _ = data.Close() }()
	bootTime, err := parseBootTime(data)
	if err != nil {
		return Observation{}, err
	}
	return Observation{BootTime: &bootTime}, nil
}

func parseBootTime(reader io.Reader) (time.Time, error) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || fields[0] != "btime" {
			continue
		}
		if len(fields) != 2 {
			return time.Time{}, errors.New("invalid /proc/stat btime field")
		}
		seconds, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || seconds <= 0 {
			return time.Time{}, errors.New("invalid /proc/stat boot time")
		}
		return time.Unix(seconds, 0).UTC(), nil
	}
	if err := scanner.Err(); err != nil {
		return time.Time{}, err
	}
	return time.Time{}, errors.New("/proc/stat has no btime field")
}

func (a *Agent) probeHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		a.mu.RLock()
		last := a.lastSuccessfulReport
		a.mu.RUnlock()
		if last.IsZero() || a.now().Sub(last) > 2*a.config.Interval+a.config.RequestLimit {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func (a *Agent) postObservation(ctx context.Context, endpoint string, body []byte, token string) error {
	requestCtx, cancel := context.WithTimeout(ctx, a.config.RequestLimit)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("build Station API request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := a.client.Do(request)
	if err != nil {
		return fmt.Errorf("send Station observation: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("Station API rejected observation with HTTP %d", response.StatusCode)
	}
	return nil
}
