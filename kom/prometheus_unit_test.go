package kom

import (
	"net/http"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
)

func TestPrometheusService_GetScheme(t *testing.T) {
	s := &PrometheusService{}

	appProtocolHttps := "https"
	appProtocolHttp := "http"

	tests := []struct {
		name     string
		svc      *corev1.Service
		port     int32
		expected string
	}{
		{
			name: "AppProtocol https",
			svc: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Port:        9090,
							AppProtocol: &appProtocolHttps,
						},
					},
				},
			},
			port:     9090,
			expected: "https",
		},
		{
			name: "AppProtocol http",
			svc: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Port:        9090,
							AppProtocol: &appProtocolHttp,
						},
					},
				},
			},
			port:     9090,
			expected: "http",
		},
		{
			name: "Port Name https",
			svc: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Port: 9090,
							Name: "my-https-port",
						},
					},
				},
			},
			port:     9090,
			expected: "https",
		},
		{
			name: "Port 443",
			svc: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Port: 443,
							Name: "web",
						},
					},
				},
			},
			port:     443,
			expected: "https",
		},
		{
			name: "Port 8443",
			svc: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Port: 8443,
							Name: "web",
						},
					},
				},
			},
			port:     8443,
			expected: "https",
		},
		{
			name: "Default http",
			svc: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Port: 9090,
							Name: "web",
						},
					},
				},
			},
			port:     9090,
			expected: "http",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.getScheme(tt.svc, tt.port)
			if got != tt.expected {
				t.Errorf("getScheme() = %v, expected %v", got, tt.expected)
			}
		})
	}
}

func TestPromClient_ApiTLSConfig(t *testing.T) {
	pcHttps := &PromClient{
		address: "https://prometheus.monitoring.svc:9090",
	}

	apiClient, err := pcHttps.api()
	if err != nil {
		t.Fatalf("failed to create api client for https: %v", err)
	}
	if apiClient == nil {
		t.Fatal("expected api client to be non-nil")
	}

	pcHttp := &PromClient{
		address: "http://prometheus.monitoring.svc:9090",
	}

	apiClientHttp, err := pcHttp.api()
	if err != nil {
		t.Fatalf("failed to create api client for http: %v", err)
	}
	if apiClientHttp == nil {
		t.Fatal("expected api client to be non-nil")
	}
}

func TestPromClient_ApiBearerToken(t *testing.T) {
	// Create a mock rest.Config with a bearer token
	config := &rest.Config{
		BearerToken: "my-mock-token",
	}

	// Create a mock cluster instance
	cluster := &ClusterInst{
		ID:     "mock-cluster",
		Config: config,
	}
	clusterInstances.clusters.Store("mock-cluster", cluster)
	defer clusterInstances.clusters.Delete("mock-cluster")

	k := &Kubectl{ID: "mock-cluster"}
	k.Statement = &Statement{Kubectl: k}

	pc := &PromClient{
		address: "http://prometheus.monitoring.svc:9090",
		service: k.Prometheus(),
	}

	apiClient, err := pc.api()
	if err != nil {
		t.Fatalf("failed to create api client with token: %v", err)
	}
	if apiClient == nil {
		t.Fatal("expected api client to be non-nil")
	}
}

func TestPromClient_ApiBearerTokenFile(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "token")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	expectedToken := "token-from-file"
	if _, err := tmpFile.WriteString(expectedToken); err != nil {
		t.Fatalf("failed to write token: %v", err)
	}
	tmpFile.Close()

	config := &rest.Config{
		BearerTokenFile: tmpFile.Name(),
	}

	cluster := &ClusterInst{
		ID:     "mock-cluster-file",
		Config: config,
	}
	clusterInstances.clusters.Store("mock-cluster-file", cluster)
	defer clusterInstances.clusters.Delete("mock-cluster-file")

	k := &Kubectl{ID: "mock-cluster-file"}
	k.Statement = &Statement{Kubectl: k}

	pc := &PromClient{
		address: "http://prometheus.monitoring.svc:9090",
		service: k.Prometheus(),
	}

	apiClient, err := pc.api()
	if err != nil {
		t.Fatalf("failed to create api client with token file: %v", err)
	}
	if apiClient == nil {
		t.Fatal("expected api client to be non-nil")
	}
}

func TestPromClient_ApiProxy(t *testing.T) {
	// Create a mock rest.Config with a bearer token
	config := &rest.Config{
		Host:        "https://100.115.101.200:6443",
		BearerToken: "my-mock-token",
	}

	// Create a mock cluster instance
	cluster := &ClusterInst{
		ID:     "mock-cluster-proxy",
		Config: config,
	}
	clusterInstances.clusters.Store("mock-cluster-proxy", cluster)
	defer clusterInstances.clusters.Delete("mock-cluster-proxy")

	k := &Kubectl{ID: "mock-cluster-proxy"}
	k.Statement = &Statement{Kubectl: k}

	pc := &PromClient{
		address: "https://100.115.101.200:6443/api/v1/namespaces/cpaas-system/services/https:cpaas-monitor-prometheus-adapter:443/proxy",
		service: k.Prometheus(),
	}

	apiClient, err := pc.api()
	if err != nil {
		t.Fatalf("failed to create api client with proxy: %v", err)
	}
	if apiClient == nil {
		t.Fatal("expected api client to be non-nil")
	}
}

func TestPromClient_ParseHelpers(t *testing.T) {
	// Test parseNamespaceAndServiceFromProxy
	testsProxy := []struct {
		addr string
		ns   string
		svc  string
	}{
		{
			addr: "https://100.115.101.200:6443/api/v1/namespaces/cpaas-system/services/https:cpaas-monitor-prometheus-adapter:443/proxy",
			ns:   "cpaas-system",
			svc:  "cpaas-monitor-prometheus-adapter",
		},
		{
			addr: "https://100.115.101.200:6443/api/v1/namespaces/monitoring/services/prometheus-k8s:9090/proxy/api/v1/query",
			ns:   "monitoring",
			svc:  "prometheus-k8s",
		},
		{
			addr: "http://localhost:8080/api/v1/namespaces/default/services/http:my-service/proxy",
			ns:   "default",
			svc:  "my-service",
		},
	}

	for _, tt := range testsProxy {
		ns, svc := parseNamespaceAndServiceFromProxy(tt.addr)
		if ns != tt.ns || svc != tt.svc {
			t.Errorf("parseNamespaceAndServiceFromProxy(%s) = (%s, %s); want (%s, %s)", tt.addr, ns, svc, tt.ns, tt.svc)
		}
	}

	// Test parseSchemeAndPort
	testsSchemePort := []struct {
		addr   string
		scheme string
		port   int
	}{
		{
			addr:   "https://cpaas-monitor-prometheus-adapter.cpaas-system.svc:443",
			scheme: "https",
			port:   443,
		},
		{
			addr:   "http://prometheus-k8s.monitoring.svc:9090",
			scheme: "http",
			port:   9090,
		},
		{
			addr:   "http://prometheus.default.svc",
			scheme: "http",
			port:   80,
		},
		{
			addr:   "https://prometheus.default.svc",
			scheme: "https",
			port:   443,
		},
	}

	for _, tt := range testsSchemePort {
		scheme, port := parseSchemeAndPort(tt.addr)
		if scheme != tt.scheme || port != tt.port {
			t.Errorf("parseSchemeAndPort(%s) = (%s, %d); want (%s, %d)", tt.addr, scheme, port, tt.scheme, tt.port)
		}
	}
}

type mockRoundTripper func(req *http.Request) (*http.Response, error)

func (m mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return m(req)
}

func TestBasicAuthRoundTripper(t *testing.T) {
	username := "admin"
	password := "secret-password"

	rt := &basicAuthRoundTripper{
		username: username,
		password: password,
		rt: mockRoundTripper(func(req *http.Request) (*http.Response, error) {
			u, p, ok := req.BasicAuth()
			if !ok {
				t.Error("Expected Basic Auth header to be set, but it was not")
			}
			if u != username || p != password {
				t.Errorf("Basic Auth credentials = (%s, %s); want (%s, %s)", u, p, username, password)
			}
			return &http.Response{StatusCode: 200}, nil
		}),
	}

	req, _ := http.NewRequest("GET", "http://example.com", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d; want 200", resp.StatusCode)
	}
}
