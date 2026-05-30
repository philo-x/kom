package kom

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestResourceUsageResult_MarshalJSON(t *testing.T) {
	// Create mock quantities
	cpuReq := resource.MustParse("100m")
	memReq := resource.MustParse("100Mi")
	cpuLim := resource.MustParse("200m")
	memLim := resource.MustParse("200Mi")
	cpuReal := resource.MustParse("2m")
	memReal := resource.MustParse("13694402560m") // 13.06Mi

	usage := &ResourceUsageResult{
		Requests: map[corev1.ResourceName]resource.Quantity{
			corev1.ResourceCPU:    cpuReq,
			corev1.ResourceMemory: memReq,
		},
		Limits: map[corev1.ResourceName]resource.Quantity{
			corev1.ResourceCPU:    cpuLim,
			corev1.ResourceMemory: memLim,
		},
		Realtime: map[corev1.ResourceName]resource.Quantity{
			corev1.ResourceCPU:    cpuReal,
			corev1.ResourceMemory: memReal,
		},
		Allocatable: map[corev1.ResourceName]resource.Quantity{
			corev1.ResourceCPU:    resource.MustParse("16"),
			corev1.ResourceMemory: resource.MustParse("32Gi"),
		},
		UsageFractions: map[corev1.ResourceName]ResourceUsageFraction{
			corev1.ResourceCPU: {
				RequestFraction:  "0.64",
				LimitFraction:    "1.27",
				RealtimeFraction: "1.00",
			},
			corev1.ResourceMemory: {
				RequestFraction:  "0.45",
				LimitFraction:    "0.91",
				RealtimeFraction: "6.53",
			},
		},
	}

	data, err := json.Marshal(usage)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	t.Logf("Serialized JSON: %s", string(data))

	// Unmarshal to a map to verify values
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	// Verify realtime memory values
	realtimeMap, ok := m["realtime"].(map[string]interface{})
	if !ok {
		t.Fatal("missing 'realtime' object")
	}
	if realtimeMap["memory"] != "13.69M" {
		t.Errorf("expected formatted realtime memory '13.69M', got '%v'", realtimeMap["memory"])
	}

	limitsMap, ok := m["limits"].(map[string]interface{})
	if !ok {
		t.Fatal("missing 'limits' object")
	}
	if limitsMap["memory"] != "200.00Mi" {
		t.Errorf("expected formatted memory limit '200.00Mi', got '%v'", limitsMap["memory"])
	}

	realtimeValueMap, ok := m["realtime_value"].(map[string]interface{})
	if !ok {
		t.Fatal("missing 'realtime_value' object")
	}
	// json numbers are parsed as float64 by default in interface{} map
	val, ok := realtimeValueMap["memory"].(float64)
	if !ok {
		t.Errorf("realtime_value.memory is not a float64: %T", realtimeValueMap["memory"])
	} else if int64(val) != 13694403 {
		t.Errorf("expected raw memory value 13694403, got %d", int64(val))
	}
}
