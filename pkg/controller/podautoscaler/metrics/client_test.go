package metrics

import (
	"context"
	"testing"

	autoscalingv1alpha1 "github.com/aibrix/aibrix/api/autoscaling/v1alpha1"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestProxy(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Metrics Suite")
}

// MetricFetcherRecorder records url used for fetching metrics.
type MetricFetcherRecorder struct {
	RestMetricsFetcher

	url string
}

func NewMetricFetcherRecorder() *MetricFetcherRecorder {
	recorder := &MetricFetcherRecorder{}
	recorder.RestMetricsFetcher.test_url_setter = func(url string) {
		recorder.url = url
	}
	return recorder
}

func GetDomainMetricSource0() autoscalingv1alpha1.MetricSource {
	return autoscalingv1alpha1.MetricSource{
		MetricSourceType: "domain",
		ProtocolType:     "http",
		Endpoint:         "example.com",
		Path:             "/metrics",
	}
}

func GetDomainMetricSource1() autoscalingv1alpha1.MetricSource {
	return autoscalingv1alpha1.MetricSource{
		MetricSourceType: "domain",
		ProtocolType:     "http",
		Endpoint:         "example.com:8080",
		Path:             "/metrics",
	}
}

func GetDomainMetricSource2() autoscalingv1alpha1.MetricSource {
	return autoscalingv1alpha1.MetricSource{
		MetricSourceType: "domain",
		ProtocolType:     "https",
		Endpoint:         "example.com",
		Port:             "8000",
		Path:             "metrics",
	}
}

func GetDomainMetricSource3() autoscalingv1alpha1.MetricSource {
	return autoscalingv1alpha1.MetricSource{
		MetricSourceType: "domain",
		ProtocolType:     "https",
		Endpoint:         "example.com:8080",
		Port:             "8000",
		Path:             "metrics",
	}
}

var _ = Describe("KPAMetricsClient", func() {
	It("should client fetch metrics from correct url", func() {
		recorder := NewMetricFetcherRecorder()
		client := NewKPAMetricsClient(recorder, 0, 0)

		client.GetMetricFromSource(context.Background(), GetDomainMetricSource0())
		Expect(recorder.url).To(Equal("http://example.com/metrics"))

		client.GetMetricFromSource(context.Background(), GetDomainMetricSource1())
		Expect(recorder.url).To(Equal("http://example.com:8080/metrics"))

		client.GetMetricFromSource(context.Background(), GetDomainMetricSource2())
		Expect(recorder.url).To(Equal("https://example.com:8000/metrics"))

		client.GetMetricFromSource(context.Background(), GetDomainMetricSource3())
		Expect(recorder.url).To(Equal("https://example.com:8080/metrics"))
	})

})
