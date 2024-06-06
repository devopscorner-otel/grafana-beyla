package otel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariomac/guara/pkg/test"
	"github.com/mariomac/pipes/pipe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/beyla/pkg/internal/imetrics"
	"github.com/grafana/beyla/pkg/internal/pipe/global"
	"github.com/grafana/beyla/pkg/internal/request"
)

var fakeMux = sync.Mutex{}

func TestHTTPMetricsEndpointOptions(t *testing.T) {
	defer restoreEnvAfterExecution()()
	mcfg := MetricsConfig{
		CommonEndpoint:  "https://localhost:3131",
		MetricsEndpoint: "https://localhost:3232/v1/metrics",
	}

	t.Run("testing with two endpoints", func(t *testing.T) {
		testMetricsHTTPOptions(t, otlpOptions{Endpoint: "localhost:3232", URLPath: "/v1/metrics"}, &mcfg)
	})

	mcfg = MetricsConfig{
		CommonEndpoint: "https://localhost:3131/otlp",
	}

	t.Run("testing with only common endpoint", func(t *testing.T) {
		testMetricsHTTPOptions(t, otlpOptions{Endpoint: "localhost:3131", URLPath: "/otlp/v1/metrics"}, &mcfg)
	})

	mcfg = MetricsConfig{
		CommonEndpoint:  "https://localhost:3131",
		MetricsEndpoint: "http://localhost:3232",
	}
	t.Run("testing with insecure endpoint", func(t *testing.T) {
		testMetricsHTTPOptions(t, otlpOptions{Endpoint: "localhost:3232", Insecure: true}, &mcfg)
	})

	mcfg = MetricsConfig{
		CommonEndpoint:     "https://localhost:3232",
		InsecureSkipVerify: true,
	}

	t.Run("testing with skip TLS verification", func(t *testing.T) {
		testMetricsHTTPOptions(t, otlpOptions{Endpoint: "localhost:3232", URLPath: "/v1/metrics", SkipTLSVerify: true}, &mcfg)
	})
}

func TestHTTPMetricsWithGrafanaOptions(t *testing.T) {
	defer restoreEnvAfterExecution()
	mcfg := MetricsConfig{Grafana: &GrafanaOTLP{
		Submit:     []string{submitMetrics, submitTraces},
		CloudZone:  "eu-west-23",
		InstanceID: "12345",
		APIKey:     "affafafaafkd",
	}}
	t.Run("testing basic Grafana Cloud options", func(t *testing.T) {
		testMetricsHTTPOptions(t, otlpOptions{
			Endpoint: "otlp-gateway-eu-west-23.grafana.net",
			URLPath:  "/otlp/v1/metrics",
			HTTPHeaders: map[string]string{
				// Basic + output of: echo -n 12345:affafafaafkd | gbase64 -w 0
				"Authorization": "Basic MTIzNDU6YWZmYWZhZmFhZmtk",
			},
		}, &mcfg)
	})
	mcfg.CommonEndpoint = "https://localhost:3939"
	t.Run("Overriding endpoint URL", func(t *testing.T) {
		testMetricsHTTPOptions(t, otlpOptions{
			Endpoint: "localhost:3939",
			URLPath:  "/v1/metrics",
			HTTPHeaders: map[string]string{
				// Basic + output of: echo -n 12345:affafafaafkd | gbase64 -w 0
				"Authorization": "Basic MTIzNDU6YWZmYWZhZmFhZmtk",
			},
		}, &mcfg)
	})
}

func testMetricsHTTPOptions(t *testing.T, expected otlpOptions, mcfg *MetricsConfig) {
	defer restoreEnvAfterExecution()()
	opts, err := getHTTPMetricEndpointOptions(mcfg)
	require.NoError(t, err)
	assert.Equal(t, expected, opts)
}

func TestMissingSchemeInMetricsEndpoint(t *testing.T) {
	defer restoreEnvAfterExecution()()
	opts, err := getHTTPMetricEndpointOptions(&MetricsConfig{CommonEndpoint: "http://foo:3030"})
	require.NoError(t, err)
	require.NotEmpty(t, opts)

	_, err = getHTTPMetricEndpointOptions(&MetricsConfig{CommonEndpoint: "foo:3030"})
	require.Error(t, err)

	_, err = getHTTPMetricEndpointOptions(&MetricsConfig{CommonEndpoint: "foo"})
	require.Error(t, err)
}

type testPipeline struct {
	inputNode pipe.Start[[]request.Span]
	exporter  pipe.Final[[]request.Span]
}

func (i *testPipeline) Connect() { i.inputNode.SendTo(i.exporter) }

func TestMetrics_InternalInstrumentation(t *testing.T) {
	defer restoreEnvAfterExecution()()
	// fake OTEL collector server
	coll := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusOK)
	}))
	defer coll.Close()
	// Wait for the HTTP server to be alive
	test.Eventually(t, timeout, func(t require.TestingT) {
		resp, err := coll.Client().Get(coll.URL + "/foo")
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	// create a simple dummy graph to send data to the Metrics reporter, which will send
	// metrics to the fake collector
	builder := pipe.NewBuilder(&testPipeline{})
	sendData := make(chan struct{})
	pipe.AddStart(builder, func(impl *testPipeline) *pipe.Start[[]request.Span] {
		return &impl.inputNode
	}, func(out chan<- []request.Span) {
		// on every send data signal, the traces generator sends a dummy trace
		for range sendData {
			out <- []request.Span{{Type: request.EventTypeHTTP}}
		}
	})

	internalMetrics := &fakeInternalMetrics{}
	pipe.AddFinalProvider(builder, func(impl *testPipeline) *pipe.Final[[]request.Span] {
		return &impl.exporter
	}, ReportMetrics(context.Background(),
		&global.ContextInfo{
			Metrics: internalMetrics,
		},
		&MetricsConfig{CommonEndpoint: coll.URL, Interval: 10 * time.Millisecond, ReportersCacheLen: 16, Features: []string{FeatureApplication}},
		nil),
	)
	graph, err := builder.Build()
	require.NoError(t, err)

	graph.Start()

	sendData <- struct{}{}
	var previousSum, previousCount int
	test.Eventually(t, timeout, func(t require.TestingT) {
		// we can't guarantee the number of calls at test time, but they must be at least 1
		previousSum, previousCount = internalMetrics.SumCount()
		assert.LessOrEqual(t, 1, previousSum)
		assert.LessOrEqual(t, 1, previousCount)
		// the count of metrics should be larger than the number of calls (1 call : n metrics)
		assert.Less(t, previousCount, previousSum)
		// no call should return error
		assert.Zero(t, internalMetrics.Errors())
	})

	sendData <- struct{}{}
	// after some time, the number of calls should be higher than before
	test.Eventually(t, timeout, func(t require.TestingT) {
		sum, cnt := internalMetrics.SumCount()
		assert.LessOrEqual(t, previousSum, sum)
		assert.LessOrEqual(t, previousCount, cnt)
		assert.Less(t, cnt, sum)
		// no call should return error
		assert.Zero(t, internalMetrics.Errors())
	})

	// collector starts failing, so errors should be received
	coll.CloseClientConnections()
	coll.Close()
	// Wait for the HTTP server to be stopped
	test.Eventually(t, timeout, func(t require.TestingT) {
		_, err := coll.Client().Get(coll.URL + "/foo")
		require.Error(t, err)
	})

	var previousErrCount int
	sendData <- struct{}{}
	test.Eventually(t, timeout, func(t require.TestingT) {
		previousSum, previousCount = internalMetrics.SumCount()
		// calls should start returning errors
		previousErrCount = internalMetrics.Errors()
		assert.NotZero(t, previousErrCount)
	})

	// after a while, metrics count should not increase but errors do
	sendData <- struct{}{}
	test.Eventually(t, timeout, func(t require.TestingT) {
		sum, cnt := internalMetrics.SumCount()
		assert.Equal(t, previousSum, sum)
		assert.Equal(t, previousCount, cnt)
		// calls should start returning errors
		assert.Less(t, previousErrCount, internalMetrics.Errors())
	})
}

type fakeInternalMetrics struct {
	imetrics.NoopReporter
	sum  atomic.Int32
	cnt  atomic.Int32
	errs atomic.Int32
}

func TestGRPCMetricsEndpointOptions(t *testing.T) {
	defer restoreEnvAfterExecution()()
	t.Run("do not accept URLs without a scheme", func(t *testing.T) {
		_, err := getGRPCMetricEndpointOptions(&MetricsConfig{CommonEndpoint: "foo:3939"})
		assert.Error(t, err)
	})

	mcfg := MetricsConfig{
		CommonEndpoint:  "https://localhost:3131",
		MetricsEndpoint: "https://localhost:3232",
	}

	t.Run("testing with two endpoints", func(t *testing.T) {
		testMetricsGRPCOptions(t, otlpOptions{Endpoint: "localhost:3232"}, &mcfg)
	})

	mcfg = MetricsConfig{
		CommonEndpoint: "https://localhost:3131",
	}

	t.Run("testing with only common endpoint", func(t *testing.T) {
		testMetricsGRPCOptions(t, otlpOptions{Endpoint: "localhost:3131"}, &mcfg)
	})

	mcfg = MetricsConfig{
		CommonEndpoint:  "https://localhost:3131",
		MetricsEndpoint: "http://localhost:3232",
	}
	t.Run("testing with insecure endpoint", func(t *testing.T) {
		testMetricsGRPCOptions(t, otlpOptions{Endpoint: "localhost:3232", Insecure: true}, &mcfg)
	})

	mcfg = MetricsConfig{
		CommonEndpoint:     "https://localhost:3232",
		InsecureSkipVerify: true,
	}

	t.Run("testing with skip TLS verification", func(t *testing.T) {
		testMetricsGRPCOptions(t, otlpOptions{Endpoint: "localhost:3232", SkipTLSVerify: true}, &mcfg)
	})
}

func testMetricsGRPCOptions(t *testing.T, expected otlpOptions, mcfg *MetricsConfig) {
	defer restoreEnvAfterExecution()()
	opts, err := getGRPCMetricEndpointOptions(mcfg)
	require.NoError(t, err)
	assert.Equal(t, expected, opts)
}

func TestMetricsSetupHTTP_Protocol(t *testing.T) {
	testCases := []struct {
		Endpoint               string
		ProtoVal               Protocol
		MetricProtoVal         Protocol
		ExpectedProtoEnv       string
		ExpectedMetricProtoEnv string
	}{
		{ProtoVal: "", MetricProtoVal: "", ExpectedProtoEnv: "", ExpectedMetricProtoEnv: "http/protobuf"},
		{ProtoVal: "", MetricProtoVal: "foo", ExpectedProtoEnv: "", ExpectedMetricProtoEnv: "foo"},
		{ProtoVal: "bar", MetricProtoVal: "", ExpectedProtoEnv: "bar", ExpectedMetricProtoEnv: ""},
		{ProtoVal: "bar", MetricProtoVal: "foo", ExpectedProtoEnv: "", ExpectedMetricProtoEnv: "foo"},
		{Endpoint: "http://foo:4317", ProtoVal: "", MetricProtoVal: "", ExpectedProtoEnv: "", ExpectedMetricProtoEnv: "grpc"},
		{Endpoint: "http://foo:4317", ProtoVal: "", MetricProtoVal: "foo", ExpectedProtoEnv: "", ExpectedMetricProtoEnv: "foo"},
		{Endpoint: "http://foo:4317", ProtoVal: "bar", MetricProtoVal: "", ExpectedProtoEnv: "bar", ExpectedMetricProtoEnv: ""},
		{Endpoint: "http://foo:4317", ProtoVal: "bar", MetricProtoVal: "foo", ExpectedProtoEnv: "", ExpectedMetricProtoEnv: "foo"},
		{Endpoint: "http://foo:14317", ProtoVal: "", MetricProtoVal: "", ExpectedProtoEnv: "", ExpectedMetricProtoEnv: "grpc"},
		{Endpoint: "http://foo:14317", ProtoVal: "", MetricProtoVal: "foo", ExpectedProtoEnv: "", ExpectedMetricProtoEnv: "foo"},
		{Endpoint: "http://foo:14317", ProtoVal: "bar", MetricProtoVal: "", ExpectedProtoEnv: "bar", ExpectedMetricProtoEnv: ""},
		{Endpoint: "http://foo:14317", ProtoVal: "bar", MetricProtoVal: "foo", ExpectedProtoEnv: "", ExpectedMetricProtoEnv: "foo"},
		{Endpoint: "http://foo:4318", ProtoVal: "", MetricProtoVal: "", ExpectedProtoEnv: "", ExpectedMetricProtoEnv: "http/protobuf"},
		{Endpoint: "http://foo:4318", ProtoVal: "", MetricProtoVal: "foo", ExpectedProtoEnv: "", ExpectedMetricProtoEnv: "foo"},
		{Endpoint: "http://foo:4318", ProtoVal: "bar", MetricProtoVal: "", ExpectedProtoEnv: "bar", ExpectedMetricProtoEnv: ""},
		{Endpoint: "http://foo:4318", ProtoVal: "bar", MetricProtoVal: "foo", ExpectedProtoEnv: "", ExpectedMetricProtoEnv: "foo"},
		{Endpoint: "http://foo:24318", ProtoVal: "", MetricProtoVal: "", ExpectedProtoEnv: "", ExpectedMetricProtoEnv: "http/protobuf"},
		{Endpoint: "http://foo:24318", ProtoVal: "", MetricProtoVal: "foo", ExpectedProtoEnv: "", ExpectedMetricProtoEnv: "foo"},
		{Endpoint: "http://foo:24318", ProtoVal: "bar", MetricProtoVal: "", ExpectedProtoEnv: "bar", ExpectedMetricProtoEnv: ""},
		{Endpoint: "http://foo:24318", ProtoVal: "bar", MetricProtoVal: "foo", ExpectedProtoEnv: "", ExpectedMetricProtoEnv: "foo"},
	}
	for _, tc := range testCases {
		t.Run(tc.Endpoint+"/"+string(tc.ProtoVal)+"/"+string(tc.MetricProtoVal), func(t *testing.T) {
			defer restoreEnvAfterExecution()()
			_, err := getHTTPMetricEndpointOptions(&MetricsConfig{
				CommonEndpoint:  "http://host:3333",
				MetricsEndpoint: tc.Endpoint,
				Protocol:        tc.ProtoVal,
				MetricsProtocol: tc.MetricProtoVal,
			})
			require.NoError(t, err)
			assert.Equal(t, tc.ExpectedProtoEnv, os.Getenv(envProtocol))
			assert.Equal(t, tc.ExpectedMetricProtoEnv, os.Getenv(envMetricsProtocol))
		})
	}
}

func TestMetricSetupHTTP_DoNotOverrideEnv(t *testing.T) {
	t.Run("setting both variables", func(t *testing.T) {
		defer restoreEnvAfterExecution()()
		require.NoError(t, os.Setenv(envProtocol, "foo-proto"))
		require.NoError(t, os.Setenv(envMetricsProtocol, "bar-proto"))
		_, err := getHTTPMetricEndpointOptions(&MetricsConfig{
			CommonEndpoint: "http://host:3333", Protocol: "foo", MetricsProtocol: "bar",
		})
		require.NoError(t, err)
		assert.Equal(t, "foo-proto", os.Getenv(envProtocol))
		assert.Equal(t, "bar-proto", os.Getenv(envMetricsProtocol))
	})
	t.Run("setting only proto env var", func(t *testing.T) {
		defer restoreEnvAfterExecution()()
		require.NoError(t, os.Setenv(envProtocol, "foo-proto"))
		_, err := getHTTPMetricEndpointOptions(&MetricsConfig{
			CommonEndpoint: "http://host:3333", Protocol: "foo",
		})
		require.NoError(t, err)
		_, ok := os.LookupEnv(envMetricsProtocol)
		assert.False(t, ok)
		assert.Equal(t, "foo-proto", os.Getenv(envProtocol))
	})
}

func TestMetricsConfig_Enabled(t *testing.T) {
	assert.True(t, (&MetricsConfig{Features: []string{FeatureApplication, FeatureNetwork}, CommonEndpoint: "foo"}).Enabled())
	assert.True(t, (&MetricsConfig{Features: []string{FeatureApplication}, MetricsEndpoint: "foo"}).Enabled())
	assert.True(t, (&MetricsConfig{Features: []string{FeatureNetwork, FeatureApplication}, Grafana: &GrafanaOTLP{Submit: []string{"traces", "metrics"}, InstanceID: "33221"}}).Enabled())
}

func TestMetricsConfig_Disabled(t *testing.T) {
	assert.False(t, (&MetricsConfig{Features: []string{FeatureApplication}}).Enabled())
	assert.False(t, (&MetricsConfig{Features: []string{FeatureApplication}, Grafana: &GrafanaOTLP{Submit: []string{"traces"}, InstanceID: "33221"}}).Enabled())
	assert.False(t, (&MetricsConfig{Features: []string{FeatureApplication}, Grafana: &GrafanaOTLP{Submit: []string{"metrics"}}}).Enabled())
	// application feature is not enabled
	assert.False(t, (&MetricsConfig{CommonEndpoint: "foo"}).Enabled())
	assert.False(t, (&MetricsConfig{MetricsEndpoint: "foo", Features: []string{FeatureNetwork}}).Enabled())
	assert.False(t, (&MetricsConfig{Grafana: &GrafanaOTLP{Submit: []string{"traces", "metrics"}, InstanceID: "33221"}}).Enabled())
}

func (f *fakeInternalMetrics) OTELMetricExport(len int) {
	fakeMux.Lock()
	defer fakeMux.Unlock()
	f.cnt.Add(1)
	f.sum.Add(int32(len))
}

func (f *fakeInternalMetrics) OTELMetricExportError(_ error) {
	fakeMux.Lock()
	defer fakeMux.Unlock()
	f.errs.Add(1)
}

func (f *fakeInternalMetrics) Errors() int {
	fakeMux.Lock()
	defer fakeMux.Unlock()
	return int(f.errs.Load())
}

func (f *fakeInternalMetrics) SumCount() (sum, count int) {
	fakeMux.Lock()
	defer fakeMux.Unlock()
	return int(f.sum.Load()), int(f.cnt.Load())
}
