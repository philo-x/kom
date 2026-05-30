package kom

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	authenticationv1 "k8s.io/api/authentication/v1"
	v1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

// PrometheusService 提供基于当前集群的 Prometheus 访问能力。
type PrometheusService struct {
	kubectl *Kubectl
}

// Prometheus 从当前 Cluster/Kubectl 构造一个 Prometheus 服务访问器。
func (k *Kubectl) Prometheus() *PrometheusService {
	return &PrometheusService{kubectl: k}
}

// PromClient 表示一个具体的 Prometheus 实例（本地 Prom / Thanos 等）。
type PromClient struct {
	service *PrometheusService
	address string
}

// WithInClusterEndpoint 按命名空间和服务名称返回当前集群下的 Prometheus 客户端。
// namespace: Prometheus Service 所在的命名空间
// svcName: Prometheus Service 的名称
func (s *PrometheusService) WithInClusterEndpoint(namespace, svcName string) *PromClient {
	return s.WithAddress(s.resolveAddress(namespace, svcName))
}

// WithAddress 使用显式地址构造一个临时 Prometheus 客户端，不依赖集群配置。
func (s *PrometheusService) WithAddress(addr string) *PromClient {
	return &PromClient{
		service: s,
		address: addr,
	}
}

// Expr 在指定 Prometheus 客户端上构造一个查询构建器。
func (c *PromClient) Expr(expr string) *PromQuery {
	return &PromQuery{
		client:        c,
		expr:          expr,
		labelMatchers: map[string]string{},
	}
}

// PromQuery 表示一次 Prometheus 查询的构建器。
type PromQuery struct {
	client *PromClient

	expr string

	queryTime *time.Time

	start *time.Time
	end   *time.Time
	step  *time.Duration

	timeout *time.Duration

	labelMatchers map[string]string
}

// WithTimeout 设置单次查询的超时时间。
func (q *PromQuery) WithTimeout(d time.Duration) *PromQuery {
	q.timeout = &d
	return q
}

// Query 在当前时间点执行瞬时查询，使用链路上通过 WithContext 设置的 context。
func (q *PromQuery) Query() (*PromResult, error) {
	ctx := q.getContext()
	apiClient, err := q.client.api()
	if err != nil {
		return nil, err
	}
	ts := time.Now()
	if q.queryTime != nil {
		ts = *q.queryTime
	}
	value, warnings, err := apiClient.Query(ctx, q.expr, ts)
	if err != nil {
		return nil, err
	}
	value = filterValueByLabels(value, q.labelMatchers)
	return &PromResult{
		value:    value,
		warnings: warnings,
	}, nil
}

// QueryAt 在指定时间点执行瞬时查询。
func (q *PromQuery) QueryAt(t time.Time) (*PromResult, error) {
	q.queryTime = &t
	return q.Query()
}

// QueryRange 执行区间查询，使用链路上的 context。
func (q *PromQuery) QueryRange(start, end time.Time, step time.Duration) (*PromResult, error) {
	ctx := q.getContext()
	apiClient, err := q.client.api()
	if err != nil {
		return nil, err
	}
	r := promv1.Range{
		Start: start,
		End:   end,
		Step:  step,
	}
	value, warnings, err := apiClient.QueryRange(ctx, q.expr, r)
	if err != nil {
		return nil, err
	}
	value = filterValueByLabels(value, q.labelMatchers)
	return &PromResult{
		value:    value,
		warnings: warnings,
	}, nil
}

// getContext 从 Kubectl.Statement 中获取链路上的 context，并应用超时设置。
func (q *PromQuery) getContext() context.Context {
	var ctx context.Context
	if q.client != nil && q.client.service != nil && q.client.service.kubectl != nil {
		ctx = q.client.service.kubectl.Statement.Context
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if q.timeout != nil {
		c, _ := context.WithTimeout(ctx, *q.timeout)
		return c
	}
	return ctx
}

type tokenRoundTripper struct {
	token string
	rt    http.RoundTripper
}

func (t *tokenRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") == "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.rt.RoundTrip(req)
}

func (c *PromClient) api() (promv1.API, error) {
	addr := c.address
	if addr == "" {
		return nil, fmt.Errorf("prometheus address is not configured")
	}
	var roundTripper http.RoundTripper = api.DefaultRoundTripper

	// 检查是否通过 Kubernetes API Server 代理查询 Prometheus
	isProxy := false
	if c.service != nil && c.service.kubectl != nil {
		restConfig := c.service.kubectl.RestConfig()
		if restConfig != nil && restConfig.Host != "" {
			// 1. 如果 addr 是集群内部域名且不包含 API Server 地址，自动重写为 API Server 代理地址
			if strings.Contains(addr, ".svc") && !strings.Contains(addr, restConfig.Host) {
				promNs, promSvc := parseNamespaceAndService(addr)
				if promNs != "" && promSvc != "" {
					scheme, port := parseSchemeAndPort(addr)
					// 构造 API Server 代理地址
					svcPart := fmt.Sprintf("%s:%s:%d", scheme, promSvc, port)
					proxyAddr := fmt.Sprintf("%s/api/v1/namespaces/%s/services/%s/proxy",
						strings.TrimSuffix(restConfig.Host, "/"),
						promNs,
						svcPart)
					klog.Infof("Rewriting Prometheus address from %s to API Server proxy address %s", addr, proxyAddr)
					addr = proxyAddr
					isProxy = true
				}
			} else if strings.Contains(addr, restConfig.Host) || strings.Contains(addr, "/proxy") {
				// 已经处于代理形式
				isProxy = true
			}

			// 2. 如果是代理模式，配置 API Server 认证传输，并跳过 TLS 证书校验
			if isProxy {
				proxyConfig := rest.CopyConfig(restConfig)
				proxyConfig.TLSClientConfig.Insecure = true
				proxyConfig.TLSClientConfig.CAData = nil
				proxyConfig.TLSClientConfig.CAFile = ""
				rt, err := rest.TransportFor(proxyConfig)
				if err == nil {
					roundTripper = rt
				} else {
					klog.Errorf("Failed to get transport for API Server proxy: %v", err)
				}
			}
		}
	}

	if !isProxy && c.service != nil && c.service.kubectl != nil {
		// 默认跳过 TLS 证书校验
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		roundTripper = tr
	}

	// 3. 无论是否是代理模式，只要是在集群上下文中且能够解析出 namespace/service，都应该尝试注入认证信息
	if c.service != nil && c.service.kubectl != nil {
		ctx := c.service.kubectl.Statement.Context
		if ctx == nil {
			ctx = context.Background()
		}

		var promNs, promSvc string
		if isProxy {
			promNs, promSvc = parseNamespaceAndServiceFromProxy(addr)
		} else {
			promNs, promSvc = parseNamespaceAndService(addr)
		}

		if promNs != "" && promSvc != "" {
			// 3.1. 优先获取 Basic Auth 凭证进行注入
			username, password, err := c.service.getBasicAuthCredentials(ctx, promNs)
			if err == nil && username != "" && password != "" {
				klog.Infof("Using Basic Auth from secret for prometheus query in namespace %s", promNs)
				roundTripper = &basicAuthRoundTripper{
					username: username,
					password: password,
					rt:       roundTripper,
				}
			} else {
				// 3.2. 如果没有 Basic Auth 凭证，退回到 ServiceAccount Token 注入
				token, err := c.service.getServiceAccountToken(ctx, promNs, promSvc)
				if err != nil {
					klog.Errorf("Failed to get ServiceAccount token for prometheus service %s/%s: %v", promNs, promSvc, err)
				} else if token != "" {
					roundTripper = &tokenRoundTripper{
						token: token,
						rt:    roundTripper,
					}
				}
			}
		}
	}

	cli, err := api.NewClient(api.Config{
		Address:      addr,
		RoundTripper: roundTripper,
	})
	if err != nil {
		return nil, err
	}
	return promv1.NewAPI(cli), nil
}

// parseNamespaceAndServiceFromProxy 从 API Server 代理地址中解析命名空间和服务名。
// 例如：https://100.115.101.200:6443/api/v1/namespaces/cpaas-system/services/https:cpaas-monitor-prometheus-adapter:443/proxy -> cpaas-system, cpaas-monitor-prometheus-adapter
func parseNamespaceAndServiceFromProxy(addr string) (string, string) {
	nsIdx := strings.Index(addr, "/namespaces/")
	svcIdx := strings.Index(addr, "/services/")
	proxyIdx := strings.Index(addr, "/proxy")
	if nsIdx == -1 || svcIdx == -1 || proxyIdx == -1 || nsIdx >= svcIdx || svcIdx >= proxyIdx {
		return "", ""
	}

	nsStart := nsIdx + len("/namespaces/")
	namespace := addr[nsStart:svcIdx]

	svcStart := svcIdx + len("/services/")
	svcPart := addr[svcStart:proxyIdx]

	if strings.HasPrefix(svcPart, "https:") {
		svcPart = svcPart[len("https:"):]
	} else if strings.HasPrefix(svcPart, "http:") {
		svcPart = svcPart[len("http:"):]
	}

	if colonIdx := strings.Index(svcPart, ":"); colonIdx != -1 {
		svcPart = svcPart[:colonIdx]
	}

	return namespace, svcPart
}

// parseSchemeAndPort 从地址中解析协议和端口
func parseSchemeAndPort(addr string) (string, int) {
	scheme := "http"
	port := 80

	parts := strings.Split(addr, "://")
	if len(parts) >= 2 {
		scheme = parts[0]
		hostPort := parts[1]
		if colonIdx := strings.Index(hostPort, ":"); colonIdx != -1 {
			portStr := hostPort[colonIdx+1:]
			// 去除可能存在的路径后缀
			if slashIdx := strings.Index(portStr, "/"); slashIdx != -1 {
				portStr = portStr[:slashIdx]
			}
			var p int
			if _, err := fmt.Sscanf(portStr, "%d", &p); err == nil {
				port = p
			}
		} else {
			if scheme == "https" {
				port = 443
			}
		}
	}
	return scheme, port
}

// parseNamespaceAndService 从 Service 地址中解析出命名空间和服务名。
// 例如：https://cpaas-monitor-prometheus-adapter.cpaas-system.svc:443 -> cpaas-system, cpaas-monitor-prometheus-adapter
func parseNamespaceAndService(addr string) (string, string) {
	parts := strings.Split(addr, "://")
	if len(parts) < 2 {
		return "", ""
	}
	hostPort := parts[1]
	host := strings.Split(hostPort, ":")[0]
	domainParts := strings.Split(host, ".")
	if len(domainParts) >= 3 && domainParts[2] == "svc" {
		return domainParts[1], domainParts[0]
	}
	return "", ""
}

// getServiceAccountToken 通过 TokenRequest API 动态为指定 ServiceAccount 生成 Token。
func (s *PrometheusService) getServiceAccountToken(ctx context.Context, namespace, svcName string) (string, error) {
	clientset := s.kubectl.Client()
	if clientset == nil {
		return "", fmt.Errorf("failed to get kubernetes clientset")
	}

	var saName string

	// 1. 尝试通过 Service 的 Selector 查找匹配的 Pods，获取 Pod 的 ServiceAccountName
	svc, err := clientset.CoreV1().Services(namespace).Get(ctx, svcName, metav1.GetOptions{})
	if err == nil && svc != nil && len(svc.Spec.Selector) > 0 {
		selector := labels.Set(svc.Spec.Selector).AsSelector().String()
		pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err == nil && len(pods.Items) > 0 {
			saName = pods.Items[0].Spec.ServiceAccountName
			klog.V(4).Infof("Found ServiceAccount %q for service %s/%s from pod %s", saName, namespace, svcName, pods.Items[0].Name)
		}
	}

	// 2. 如果通过 Pod selector 没找到，尝试寻找同名的 Deployment
	if saName == "" {
		deploy, err := clientset.AppsV1().Deployments(namespace).Get(ctx, svcName, metav1.GetOptions{})
		if err == nil && deploy != nil {
			saName = deploy.Spec.Template.Spec.ServiceAccountName
			klog.V(4).Infof("Found ServiceAccount %q for service %s/%s from deployment", saName, namespace, svcName)
		}
	}

	// 3. 如果还是没有，默认使用服务名作为 ServiceAccount 名字
	if saName == "" {
		saName = svcName
	}

	klog.Infof("Requesting token for ServiceAccount %s/%s (service: %s)", namespace, saName, svcName)

	expiration := int64(3600)
	tr := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			ExpirationSeconds: &expiration,
		},
	}
	tokenReq, err := clientset.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, saName, tr, metav1.CreateOptions{})
	if err != nil {
		klog.Warningf("Failed to create token for ServiceAccount %s/%s: %v. Retrying with default ServiceAccount", namespace, saName, err)
		if saName != "default" {
			tokenReq, err = clientset.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, "default", tr, metav1.CreateOptions{})
			if err != nil {
				klog.Errorf("Failed to create token for default ServiceAccount in %s: %v", namespace, err)
			}
		}
	}
	if err != nil {
		return "", err
	}
	return tokenReq.Status.Token, nil
}

// resolveAddress 根据命名空间和服务名称解析 Prometheus 地址。
// namespace: Prometheus Service 所在的命名空间
// svcName: Prometheus Service 的名称
func (s *PrometheusService) resolveAddress(namespace, svcName string) string {
	if s == nil || s.kubectl == nil {
		return ""
	}
	if namespace == "" || svcName == "" {
		return ""
	}

	ctx := context.Background()
	if s.kubectl.Statement != nil && s.kubectl.Statement.Context != nil {
		ctx = s.kubectl.Statement.Context
	}

	// 1. 优先根据指定的命名空间和服务名称查找 Prometheus Service 并构造集群内访问地址
	var svc v1.Service
	err := s.kubectl.newInstance().WithContext(ctx).
		Resource(&v1.Service{}).
		Namespace(namespace).
		Name(svcName).
		Get(&svc).Error

	if err == nil && svc.Name != "" {
		port := s.findPrometheusPort(&svc)
		if port > 0 {
			scheme := s.getScheme(&svc, port)
			klog.Infof("Resolved internal Prometheus address via Service: %s://%s.%s.svc:%d", scheme, svc.Name, svc.Namespace, port)
			// 格式：scheme://service-name.namespace.svc:port
			return fmt.Sprintf("%s://%s.%s.svc:%d", scheme, svc.Name, svc.Namespace, port)
		}
	}

	// 2. 降级：尝试获取外部 Ingress 访问地址
	extAddr := s.resolveExternalAddress(ctx, namespace, svcName)
	if extAddr != "" {
		klog.Infof("Resolved external Prometheus address via Ingress: %s", extAddr)
		return extAddr
	}

	return ""
}

// resolveExternalAddress 尝试在指定的命名空间下寻找 Prometheus 相关的 Ingress 地址并拼接
func (s *PrometheusService) resolveExternalAddress(ctx context.Context, namespace, svcName string) string {
	clientset := s.kubectl.Client()
	if clientset == nil {
		return ""
	}
	// 查找 Ingress 列表
	var ingresses *networkingv1.IngressList
	var err error
	ingresses, err = clientset.NetworkingV1().Ingresses(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return ""
	}

	// 寻找最匹配的 Ingress
	for _, ing := range ingresses.Items {
		// 检查 Ingress 是否指向我们的 Service
		for _, rule := range ing.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}
			for _, path := range rule.HTTP.Paths {
				if path.Backend.Service != nil && path.Backend.Service.Name == svcName {
					// 找到匹配的 Ingress!
					host := rule.Host
					if host == "" {
						// 尝试从 Status.LoadBalancer 中获取 IP/Hostname
						if len(ing.Status.LoadBalancer.Ingress) > 0 {
							host = ing.Status.LoadBalancer.Ingress[0].IP
							if host == "" {
								host = ing.Status.LoadBalancer.Ingress[0].Hostname
							}
						}
					}

					// 如果还是没有 host，使用 restConfig 的 Host IP
					if host == "" {
						restConfig := s.kubectl.RestConfig()
						if restConfig != nil && restConfig.Host != "" {
							u, err := url.Parse(restConfig.Host)
							if err == nil {
								host = u.Host
							} else {
								host = restConfig.Host
							}
						}
					}

					if host == "" {
						continue
					}

					// 确保 host 包含 http/https
					scheme := "http"
					if len(ing.Spec.TLS) > 0 {
						scheme = "https"
					}

					if !strings.Contains(host, "://") {
						host = fmt.Sprintf("%s://%s", scheme, host)
					}

					pathStr := path.Path
					return fmt.Sprintf("%s%s", strings.TrimSuffix(host, "/"), pathStr)
				}
			}
		}
	}
	return ""
}

// getScheme 根据 Service 端口属性判断使用 http 还是 https 协议。
func (s *PrometheusService) getScheme(svc *v1.Service, port int32) string {
	if svc == nil {
		return "http"
	}
	for _, p := range svc.Spec.Ports {
		if p.Port == port {
			if p.AppProtocol != nil && strings.EqualFold(*p.AppProtocol, "https") {
				return "https"
			}
			if strings.Contains(strings.ToLower(p.Name), "https") {
				return "https"
			}
		}
	}
	if port == 443 || port == 8443 || port == 6443 {
		return "https"
	}
	return "http"
}

// findPrometheusPort 从 Service 中查找 Prometheus 的端口号。
// 通常 Prometheus 使用 9090 端口，或者端口名为 web/http/prometheus/https。
func (s *PrometheusService) findPrometheusPort(svc *v1.Service) int32 {
	if svc == nil || len(svc.Spec.Ports) == 0 {
		return 0
	}

	// 常见的 Prometheus 端口名称
	preferredPortNames := []string{"web", "http", "prometheus", "metrics", "https"}

	// 优先查找命名端口
	for _, portName := range preferredPortNames {
		for _, port := range svc.Spec.Ports {
			if strings.EqualFold(port.Name, portName) {
				return port.Port
			}
		}
	}

	// 查找 9090 端口（Prometheus 默认端口）
	for _, port := range svc.Spec.Ports {
		if port.Port == 9090 {
			return port.Port
		}
	}

	// 如果只有一个端口，使用该端口
	if len(svc.Spec.Ports) == 1 {
		return svc.Spec.Ports[0].Port
	}

	// 都不匹配，使用第一个端口
	if len(svc.Spec.Ports) > 0 {
		return svc.Spec.Ports[0].Port
	}

	return 0
}

// PromResult 封装 Prometheus 查询返回的结果。
type PromResult struct {
	value    model.Value
	warnings promv1.Warnings
}

// Raw 返回底层的 Prometheus model.Value 与 warnings。
func (r *PromResult) Raw() (model.Value, promv1.Warnings) {
	if r == nil {
		return nil, nil
	}
	return r.value, r.warnings
}

// Sample 表示一个时间序列样本点（向量模式下）。
type Sample struct {
	Metric    map[string]string
	Value     float64
	Timestamp time.Time
}

// Series 表示一条时间序列（矩阵模式下）。
type Series struct {
	Metric  map[string]string
	Samples []SamplePoint
}

// SamplePoint 表示矩阵模式下每个时间点的值。
type SamplePoint struct {
	Timestamp time.Time
	Value     float64
}

// AsScalar 试图将结果解析为单个标量值。
func (r *PromResult) AsScalar() (float64, bool) {
	if r == nil || r.value == nil {
		return 0, false
	}
	switch v := r.value.(type) {
	case *model.Scalar:
		return float64(v.Value), true
	case model.Vector:
		if len(v) == 1 {
			return float64(v[0].Value), true
		}
	}
	return 0, false
}

// AsVector 将结果转换为 Sample 列表（仅在结果为向量时有效）。
func (r *PromResult) AsVector() []Sample {
	if r == nil || r.value == nil {
		return nil
	}
	vec, ok := r.value.(model.Vector)
	if !ok {
		return nil
	}
	out := make([]Sample, 0, len(vec))
	for _, s := range vec {
		out = append(out, Sample{
			Metric:    metricToMap(s.Metric),
			Value:     float64(s.Value),
			Timestamp: s.Timestamp.Time(),
		})
	}
	return out
}

// AsMatrix 将结果转换为 Series 列表（仅在结果为矩阵时有效）。
func (r *PromResult) AsMatrix() []Series {
	if r == nil || r.value == nil {
		return nil
	}
	mat, ok := r.value.(model.Matrix)
	if !ok {
		return nil
	}
	out := make([]Series, 0, len(mat))
	for _, s := range mat {
		series := Series{
			Metric:  metricToMap(s.Metric),
			Samples: make([]SamplePoint, 0, len(s.Values)),
		}
		for _, v := range s.Values {
			series.Samples = append(series.Samples, SamplePoint{
				Timestamp: v.Timestamp.Time(),
				Value:     float64(v.Value),
			})
		}
		out = append(out, series)
	}
	return out
}

// AsString 以字符串形式返回结果，便于调试。
func (r *PromResult) AsString() string {
	if r == nil || r.value == nil {
		return ""
	}
	return r.value.String()
}

// metricToMap 将 Prometheus 的 Metric 转换为普通 map。
func metricToMap(m model.Metric) map[string]string {
	if len(m) == 0 {
		return nil
	}
	res := make(map[string]string, len(m))
	for k, v := range m {
		res[string(k)] = string(v)
	}
	return res
}

// filterValueByLabels 根据 labelMatchers 对 Vector/Matrix 结果做过滤。
func filterValueByLabels(v model.Value, matchers map[string]string) model.Value {
	if len(matchers) == 0 || v == nil {
		return v
	}
	switch typed := v.(type) {
	case model.Vector:
		out := make(model.Vector, 0, len(typed))
		for _, s := range typed {
			if metricMatches(s.Metric, matchers) {
				out = append(out, s)
			}
		}
		return out
	case model.Matrix:
		out := make(model.Matrix, 0, len(typed))
		for _, s := range typed {
			if metricMatches(s.Metric, matchers) {
				out = append(out, s)
			}
		}
		return out
	default:
		return v
	}
}

// metricMatches 判断某条 Metric 是否满足所有 label 匹配条件。
func metricMatches(m model.Metric, matchers map[string]string) bool {
	for k, v := range matchers {
		if m[model.LabelName(k)] != model.LabelValue(v) {
			return false
		}
	}
	return true
}

// QueryScalar 执行瞬时查询并期望返回单个标量结果。
func (q *PromQuery) QueryScalar() (float64, error) {
	res, err := q.Query()
	if err != nil {
		return 0, err
	}
	val, ok := res.AsScalar()
	if !ok {
		return 0, fmt.Errorf("result is not scalar")
	}
	return val, nil
}

// QueryVector 执行瞬时查询并将结果直接转换为 Sample 列表。
func (q *PromQuery) QueryVector() ([]Sample, error) {
	res, err := q.Query()
	if err != nil {
		return nil, err
	}
	return res.AsVector(), nil
}

// QueryMatrix 执行瞬时查询并将结果直接转换为 Series 列表。
func (q *PromQuery) QueryMatrix() ([]Series, error) {
	res, err := q.Query()
	if err != nil {
		return nil, err
	}
	return res.AsMatrix(), nil
}

type basicAuthRoundTripper struct {
	username string
	password string
	rt       http.RoundTripper
}

func (b *basicAuthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	reqClone := new(http.Request)
	*reqClone = *req
	reqClone.Header = make(http.Header, len(req.Header))
	for k, s := range req.Header {
		reqClone.Header[k] = append([]string(nil), s...)
	}
	reqClone.SetBasicAuth(b.username, b.password)
	return b.rt.RoundTrip(reqClone)
}

// getBasicAuthCredentials 尝试在指定的命名空间下寻找 Prometheus 相关的 Basic Auth 密文并提取账号密码
func (s *PrometheusService) getBasicAuthCredentials(ctx context.Context, namespace string) (string, string, error) {
	clientset := s.kubectl.Client()
	if clientset == nil {
		return "", "", fmt.Errorf("failed to get kubernetes clientset")
	}

	// 查找该命名空间下的所有 secrets
	secrets, err := clientset.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", "", err
	}

	// 寻找最匹配的 basic-auth secret
	var targetSecret *v1.Secret
	for _, sec := range secrets.Items {
		name := strings.ToLower(sec.Name)
		// 寻找包含 prometheus 或 thanos 且包含 basic-auth 的 secret
		if (strings.Contains(name, "prometheus") || strings.Contains(name, "thanos")) && strings.Contains(name, "basic-auth") {
			targetSecret = &sec
			break
		}
	}

	if targetSecret == nil {
		return "", "", fmt.Errorf("no prometheus basic-auth secret found")
	}

	// 从 secret 中提取 username 和 password
	usernameBytes, hasUser := targetSecret.Data["username"]
	passwordBytes, hasPass := targetSecret.Data["password"]
	if !hasUser || !hasPass {
		// 尝试其它的 key
		usernameBytes, hasUser = targetSecret.Data["user"]
		passwordBytes, hasPass = targetSecret.Data["pass"]
	}

	if !hasUser || !hasPass {
		return "", "", fmt.Errorf("username or password not found in secret %s", targetSecret.Name)
	}

	return string(usernameBytes), string(passwordBytes), nil
}
