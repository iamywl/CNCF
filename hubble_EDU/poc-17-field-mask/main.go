// Package main은 Hubble의 Field Mask 필터링 시스템을
// Go 표준 라이브러리만으로 시뮬레이션하는 PoC이다.
//
// 시뮬레이션하는 핵심 개념:
// 1. FieldMask 경로 파싱 (점 구분 중첩 경로)
// 2. 경로 트리 구성 (효율적 필터링)
// 3. 중첩된 맵 구조에서 선택적 필드 추출
// 4. 기본 FieldMask 경로 (defaults.FieldMask)
// 5. 경로 검증 (Flow 메시지 스키마 기반)
// 6. 필터링 전후 크기 비교
// 7. CLI 옵션 통합 (--experimental-field-mask)
// 8. 출력 형식별 마스크 호환성
// 9. Experimental → 최상위 마이그레이션
// 10. 대역폭 절감 효과 시뮬레이션
//
// 실제 소스 참조:
//   - api/v1/observer/observer.proto         (field_mask 필드)
//   - hubble/cmd/observe/observe.go          (CLI 플래그)
//   - hubble/cmd/observe/flows.go            (요청 구성)
//   - hubble/pkg/defaults/defaults.go        (기본 FieldMask)
//   - google.golang.org/protobuf/types/known/fieldmaskpb
package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ============================================================================
// 1. Flow 데이터 모델 (flow.Flow protobuf 시뮬레이션)
// ============================================================================

// Flow는 Hubble의 네트워크 흐름 이벤트를 JSON-like 맵으로 표현한다.
// 실제로는 protobuf flow.Flow 메시지다.
type Flow map[string]interface{}

// SampleFlow는 테스트용 전체 Flow 데이터를 생성한다.
func SampleFlow() Flow {
	return Flow{
		"time":    "2024-01-15T10:30:00Z",
		"verdict": "FORWARDED",
		"drop_reason": 0,
		"ethernet": map[string]interface{}{
			"source":      "aa:bb:cc:dd:ee:ff",
			"destination": "11:22:33:44:55:66",
		},
		"IP": map[string]interface{}{
			"source":      "10.0.1.15",
			"destination": "10.0.2.30",
			"ipVersion":   "IPv4",
			"encrypted":   false,
		},
		"l4": map[string]interface{}{
			"TCP": map[string]interface{}{
				"source_port":      uint32(45678),
				"destination_port": uint32(80),
				"flags": map[string]interface{}{
					"SYN": true,
				},
			},
		},
		"source": map[string]interface{}{
			"id":        100,
			"identity":  12345,
			"namespace": "default",
			"labels":    []string{"app=frontend", "tier=web"},
			"pod_name":  "frontend-abc123",
			"workloads": []map[string]interface{}{
				{"name": "frontend", "kind": "Deployment"},
			},
		},
		"destination": map[string]interface{}{
			"id":        200,
			"identity":  67890,
			"namespace": "default",
			"labels":    []string{"app=backend", "tier=api"},
			"pod_name":  "backend-xyz789",
			"workloads": []map[string]interface{}{
				{"name": "backend", "kind": "Deployment"},
			},
		},
		"Type":      "L3_L4",
		"node_name": "k8s-worker-01",
		"source_names":      []string{"frontend.default.svc.cluster.local"},
		"destination_names": []string{"backend.default.svc.cluster.local"},
		"l7":        nil,
		"is_reply":  false,
		"event_type": map[string]interface{}{
			"type":    4,
			"sub_type": 0,
		},
		"source_service": map[string]interface{}{
			"name":      "frontend",
			"namespace": "default",
		},
		"destination_service": map[string]interface{}{
			"name":      "backend",
			"namespace": "default",
		},
		"traffic_direction": "EGRESS",
		"trace_context": map[string]interface{}{
			"parent": map[string]interface{}{
				"trace_id": "abc123def456",
				"span_id":  "789012",
			},
		},
		"Summary":           "TCP Flags: SYN",
		"socket_cookie":     uint64(12345678),
		"cgroup_id":         uint64(4096),
		"drop_reason_desc":  "POLICY_DENIED",
		"is_l7_lb":          false,
		"auth_type":         "DISABLED",
		"ip_version":        "IPv4",
		"extensions":        nil,
	}
}

// ============================================================================
// 2. FieldMask 경로 트리
// ============================================================================

// PathTree는 FieldMask 경로를 트리 구조로 표현한다.
// 효율적인 필터링을 위해 경로를 트리로 구성한다.
type PathTree struct {
	IsLeaf   bool
	Children map[string]*PathTree
}

// NewPathTree는 경로 목록에서 PathTree를 구성한다.
func NewPathTree(paths []string) *PathTree {
	root := &PathTree{Children: make(map[string]*PathTree)}

	for _, path := range paths {
		parts := strings.Split(path, ".")
		node := root
		for _, part := range parts {
			if node.Children == nil {
				node.Children = make(map[string]*PathTree)
			}
			child, ok := node.Children[part]
			if !ok {
				child = &PathTree{Children: make(map[string]*PathTree)}
				node.Children[part] = child
			}
			node = child
		}
		node.IsLeaf = true
	}

	return root
}

// Contains는 경로가 트리에 포함되는지 확인한다.
func (t *PathTree) Contains(path string) bool {
	parts := strings.Split(path, ".")
	node := t
	for _, part := range parts {
		child, ok := node.Children[part]
		if !ok {
			return false
		}
		if child.IsLeaf {
			return true
		}
		node = child
	}
	return node.IsLeaf
}

// Print는 트리를 시각적으로 출력한다.
func (t *PathTree) Print(prefix string, indent string) {
	for name, child := range t.Children {
		marker := ""
		if child.IsLeaf && len(child.Children) == 0 {
			marker = " (leaf)"
		}
		fmt.Printf("%s%s%s\n", indent, name, marker)
		child.Print(name, indent+"  ")
	}
}

// ============================================================================
// 3. FieldMask 필터링
// ============================================================================

// ApplyFieldMask는 Flow에서 FieldMask에 지정된 필드만 추출한다.
func ApplyFieldMask(flow Flow, paths []string) Flow {
	if len(paths) == 0 {
		return flow // 마스크 없으면 전체 반환
	}

	tree := NewPathTree(paths)
	result := make(Flow)
	applyMaskRecursive(flow, result, tree)
	return result
}

func applyMaskRecursive(src, dst Flow, tree *PathTree) {
	for name, child := range tree.Children {
		srcVal, ok := src[name]
		if !ok {
			continue
		}

		if child.IsLeaf {
			// 리프 노드: 전체 값 복사
			dst[name] = srcVal
		} else if len(child.Children) > 0 {
			// 중간 노드: 재귀적으로 하위 필드만 추출
			if srcMap, ok := srcVal.(map[string]interface{}); ok {
				dstMap := make(Flow)
				applyMaskRecursive(Flow(srcMap), dstMap, child)
				if len(dstMap) > 0 {
					dst[name] = map[string]interface{}(dstMap)
				}
			}
		}
	}
}

// ============================================================================
// 4. 경로 검증 (Flow 스키마 기반)
// ============================================================================

// FlowSchema는 Flow 메시지의 필드 스키마를 정의한다.
var FlowSchema = map[string]interface{}{
	"time": "timestamp", "verdict": "enum", "drop_reason": "uint32",
	"ethernet": map[string]interface{}{
		"source": "string", "destination": "string",
	},
	"IP": map[string]interface{}{
		"source": "string", "destination": "string",
		"ipVersion": "enum", "encrypted": "bool",
	},
	"l4": map[string]interface{}{
		"TCP": map[string]interface{}{
			"source_port": "uint32", "destination_port": "uint32",
			"flags": map[string]interface{}{"SYN": "bool", "ACK": "bool"},
		},
		"UDP": map[string]interface{}{
			"source_port": "uint32", "destination_port": "uint32",
		},
	},
	"source": map[string]interface{}{
		"id": "uint32", "identity": "uint32", "namespace": "string",
		"labels": "[]string", "pod_name": "string",
		"workloads": "[]Workload",
	},
	"destination": map[string]interface{}{
		"id": "uint32", "identity": "uint32", "namespace": "string",
		"labels": "[]string", "pod_name": "string",
		"workloads": "[]Workload",
	},
	"Type": "enum", "node_name": "string",
	"l7": "Layer7", "is_reply": "BoolValue",
	"event_type": map[string]interface{}{"type": "int32", "sub_type": "int32"},
	"source_service": map[string]interface{}{
		"name": "string", "namespace": "string",
	},
	"destination_service": map[string]interface{}{
		"name": "string", "namespace": "string",
	},
	"traffic_direction": "enum", "Summary": "string",
	"trace_context": map[string]interface{}{
		"parent": map[string]interface{}{"trace_id": "string", "span_id": "string"},
	},
	"socket_cookie": "uint64", "cgroup_id": "uint64",
	"drop_reason_desc": "enum", "is_l7_lb": "bool",
	"auth_type": "enum", "ip_version": "enum",
	"extensions": "Any",
}

// ValidatePaths는 경로가 유효한지 검증한다.
func ValidatePaths(paths []string) []error {
	var errs []error
	for _, path := range paths {
		if !validatePath(path, FlowSchema) {
			errs = append(errs, fmt.Errorf("유효하지 않은 경로: %q", path))
		}
	}
	return errs
}

func validatePath(path string, schema map[string]interface{}) bool {
	parts := strings.Split(path, ".")
	current := schema
	for i, part := range parts {
		val, ok := current[part]
		if !ok {
			return false
		}
		if i == len(parts)-1 {
			return true // 경로의 마지막 부분 도달
		}
		if subSchema, ok := val.(map[string]interface{}); ok {
			current = subSchema
		} else {
			return false // 중간 경로가 중첩 메시지가 아님
		}
	}
	return true
}

// ============================================================================
// 5. 기본 FieldMask (defaults.FieldMask 시뮬레이션)
// ============================================================================

// DefaultFieldMask는 Hubble의 기본 필드 마스크다.
// 소스: hubble/pkg/defaults/defaults.go
var DefaultFieldMask = []string{
	"time", "source.identity", "source.namespace", "source.pod_name",
	"destination.identity", "destination.namespace", "destination.pod_name",
	"source_service", "destination_service",
	"l4", "IP", "ethernet", "l7",
	"Type", "node_name", "is_reply", "event_type", "verdict", "Summary",
}

// ============================================================================
// 6. 크기 비교
// ============================================================================

func jsonSize(v interface{}) int {
	b, _ := json.Marshal(v)
	return len(b)
}

// ============================================================================
// main
// ============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Hubble Field Mask 필터링 시뮬레이션 PoC                     ║")
	fmt.Println("║  실제 소스: observer.proto, observe.go, defaults.go          ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	flow := SampleFlow()
	originalSize := jsonSize(flow)

	// === 1. 전체 Flow ===
	fmt.Println("=== 1. 전체 Flow (마스크 없음) ===")
	fullJSON, _ := json.MarshalIndent(flow, "  ", "  ")
	fmt.Printf("  크기: %d bytes\n", originalSize)
	fmt.Printf("  필드 수: %d (최상위)\n", len(flow))
	fmt.Println()

	// === 2. 기본 FieldMask ===
	fmt.Println("=== 2. 기본 FieldMask (defaults.FieldMask) ===")
	fmt.Printf("  경로 (%d개):\n", len(DefaultFieldMask))
	for _, p := range DefaultFieldMask {
		fmt.Printf("    - %s\n", p)
	}
	fmt.Println()

	filtered := ApplyFieldMask(flow, DefaultFieldMask)
	filteredSize := jsonSize(filtered)
	filteredJSON, _ := json.MarshalIndent(filtered, "  ", "  ")
	fmt.Printf("  필터링 후:\n  %s\n", string(filteredJSON))
	fmt.Printf("  크기: %d bytes (원본 %d bytes, %.0f%% 절감)\n",
		filteredSize, originalSize, float64(originalSize-filteredSize)/float64(originalSize)*100)
	fmt.Println()

	// === 3. 최소 FieldMask ===
	fmt.Println("=== 3. 최소 FieldMask (커스텀) ===")
	minMask := []string{"source.pod_name", "destination.pod_name", "verdict"}
	fmt.Printf("  경로: %v\n", minMask)

	minimal := ApplyFieldMask(flow, minMask)
	minimalSize := jsonSize(minimal)
	minimalJSON, _ := json.MarshalIndent(minimal, "  ", "  ")
	fmt.Printf("  필터링 후:\n  %s\n", string(minimalJSON))
	fmt.Printf("  크기: %d bytes (원본 %d bytes, %.0f%% 절감)\n",
		minimalSize, originalSize, float64(originalSize-minimalSize)/float64(originalSize)*100)
	fmt.Println()

	// === 4. 네트워크 정책 디버깅 마스크 ===
	fmt.Println("=== 4. 네트워크 정책 디버깅 FieldMask ===")
	policyMask := []string{
		"source.pod_name", "source.identity",
		"destination.pod_name", "destination.identity",
		"verdict", "drop_reason_desc", "Summary",
	}
	policyFiltered := ApplyFieldMask(flow, policyMask)
	policyJSON, _ := json.MarshalIndent(policyFiltered, "  ", "  ")
	policySize := jsonSize(policyFiltered)
	fmt.Printf("  %s\n", string(policyJSON))
	fmt.Printf("  크기: %d bytes (%.0f%% 절감)\n",
		policySize, float64(originalSize-policySize)/float64(originalSize)*100)
	fmt.Println()

	// === 5. 경로 트리 시각화 ===
	fmt.Println("=== 5. 경로 트리 시각화 ===")
	tree := NewPathTree(DefaultFieldMask)
	tree.Print("", "  ")
	fmt.Println()

	// === 6. 경로 검증 ===
	fmt.Println("=== 6. 경로 검증 ===")
	testPaths := []string{
		"source.pod_name",     // 유효
		"destination.identity", // 유효
		"l4.TCP.destination_port", // 유효
		"source.invalid_field",    // 무효
		"nonexistent",             // 무효
		"IP.source",               // 유효
	}
	for _, p := range testPaths {
		errs := ValidatePaths([]string{p})
		status := "유효"
		if len(errs) > 0 {
			status = "무효"
		}
		fmt.Printf("  %-35s → %s\n", p, status)
	}
	fmt.Println()

	// === 7. 대역폭 절감 시뮬레이션 ===
	fmt.Println("=== 7. 대역폭 절감 시뮬레이션 (초당 100,000 flows) ===")
	flowsPerSecond := 100000
	scenarios := []struct {
		name  string
		paths []string
	}{
		{"마스크 없음 (전체)", nil},
		{"기본 마스크 (19개)", DefaultFieldMask},
		{"최소 마스크 (3개)", minMask},
	}

	for _, s := range scenarios {
		var size int
		if len(s.paths) == 0 {
			size = originalSize
		} else {
			size = jsonSize(ApplyFieldMask(flow, s.paths))
		}
		bandwidth := float64(size) * float64(flowsPerSecond) / 1024 / 1024
		fmt.Printf("  %-25s: %5d bytes/flow × %d flows/s = %.1f MB/s\n",
			s.name, size, flowsPerSecond, bandwidth)
	}
	fmt.Println()

	// === 8. 출력 형식 호환성 ===
	fmt.Println("=== 8. 출력 형식 호환성 ===")
	formats := []struct {
		name         string
		customMask   bool
		defaultMask  bool
	}{
		{"json", true, true},
		{"jsonpb", true, true},
		{"compact", false, true},
		{"dict", false, true},
		{"tab", false, true},
	}
	fmt.Printf("  %-10s  커스텀마스크  기본마스크\n", "형식")
	fmt.Printf("  %-10s  ----------  --------\n", "----")
	for _, f := range formats {
		custom := "X"
		if f.customMask {
			custom = "O"
		}
		def := "X"
		if f.defaultMask {
			def = "O"
		}
		fmt.Printf("  %-10s  %-10s  %s\n", f.name, custom, def)
	}
	fmt.Println()

	// === 9. Experimental → 최상위 마이그레이션 ===
	fmt.Println("=== 9. Field Mask 마이그레이션 ===")
	fmt.Println("  기존 (deprecated):")
	fmt.Println("    req.Experimental.FieldMask = fm")
	fmt.Println("  신규 (최상위):")
	fmt.Println("    req.FieldMask = fm")
	fmt.Println("  서버: 최상위 우선, 없으면 Experimental 확인")

	// 전체 Flow JSON 출력 (참고용)
	_ = fullJSON
}
