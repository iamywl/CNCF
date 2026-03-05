// poc-05-gitops-diff: Argo CD GitOps Engine의 diff 시스템 시뮬레이션
//
// 실제 소스 참조:
//   - gitops-engine/pkg/diff/diff.go: DiffResult, ThreeWayDiff, TwoWayDiff, Diff()
//   - gitops-engine/pkg/diff/diff.go:42-50: DiffResult struct (Modified, NormalizedLive, PredictedLive)
//   - gitops-engine/pkg/diff/diff.go:76-134: Diff() — diff 모드 결정 트리
//   - gitops-engine/pkg/diff/diff.go:689-724: ThreeWayDiff() 알고리즘
//   - gitops-engine/pkg/diff/diff.go:36: AnnotationLastAppliedConfig 상수
//
// go run main.go
package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// ─────────────────────────────────────────────
// 핵심 데이터 구조
// 실제: gitops-engine/pkg/diff/diff.go:42-50
// ─────────────────────────────────────────────

// DiffResult는 두 리소스 비교 결과를 담는다.
// Modified: 리소스가 일치하지 않으면 true
// NormalizedLive: 정규화가 적용된 실제 상태 (YAML/JSON)
// PredictedLive: 예측 상태 — config를 live에 적용했을 때의 기대 상태
type DiffResult struct {
	Modified       bool
	NormalizedLive []byte
	PredictedLive  []byte
}

// DiffResultList는 여러 리소스 비교 결과 집합
type DiffResultList struct {
	Diffs    []DiffResult
	Modified bool
}

// ─────────────────────────────────────────────
// Diff 모드 (4가지)
// 실제: gitops-engine/pkg/diff/diff.go:76-134
// ─────────────────────────────────────────────

// DiffMode는 diff 계산 방식을 나타낸다
type DiffMode int

const (
	// ServerSideDiff: k8s API 서버에 dry-run apply 후 비교 (가장 정확하지만 API 콜 필요)
	DiffModeServerSide DiffMode = iota
	// StructuredMergeDiff: Server-Side Apply 어노테이션이 있거나 옵션으로 활성화될 때
	DiffModeStructuredMerge
	// ThreeWayDiff: last-applied-configuration 어노테이션이 있을 때 3-way merge
	DiffModeThreeWay
	// TwoWayDiff: fallback — orig을 config로 대체한 변형된 ThreeWayDiff
	DiffModeTwoWay
)

func (m DiffMode) String() string {
	switch m {
	case DiffModeServerSide:
		return "ServerSideDiff"
	case DiffModeStructuredMerge:
		return "StructuredMergeDiff"
	case DiffModeThreeWay:
		return "ThreeWayDiff"
	default:
		return "TwoWayDiff"
	}
}

// ─────────────────────────────────────────────
// 리소스 표현 (Kubernetes Unstructured 단순화)
// ─────────────────────────────────────────────

// Resource는 쿠버네티스 리소스를 map으로 표현한다
type Resource map[string]interface{}

// AnnotationLastAppliedConfig는 kubectl/Argo CD가 마지막으로 적용한 설정을 저장하는 어노테이션
// 실제: gitops-engine/pkg/diff/diff.go:38
const AnnotationLastAppliedConfig = "kubectl.kubernetes.io/last-applied-configuration"

// AnnotationSyncOptions는 sync 옵션을 담는 어노테이션
const AnnotationSyncOptions = "argocd.argoproj.io/sync-options"

// clone은 Resource를 깊은 복사한다
func clone(r Resource) Resource {
	if r == nil {
		return nil
	}
	b, _ := json.Marshal(r)
	var out Resource
	_ = json.Unmarshal(b, &out)
	return out
}

// getAnnotation은 리소스에서 어노테이션 값을 반환한다
func getAnnotation(r Resource, key string) string {
	meta, ok := r["metadata"].(map[string]interface{})
	if !ok {
		return ""
	}
	annotations, ok := meta["annotations"].(map[string]interface{})
	if !ok {
		return ""
	}
	v, _ := annotations[key].(string)
	return v
}

// getLastAppliedConfig는 리소스의 last-applied-configuration 어노테이션에서
// 원본(orig) 설정을 파싱해 반환한다
// 실제: gitops-engine/pkg/diff/diff.go:GetLastAppliedConfigAnnotation()
func getLastAppliedConfig(live Resource) Resource {
	annotation := getAnnotation(live, AnnotationLastAppliedConfig)
	if annotation == "" {
		return nil
	}
	var orig Resource
	if err := json.Unmarshal([]byte(annotation), &orig); err != nil {
		return nil
	}
	return orig
}

// hasSSAAnnotation은 Server-Side Apply 어노테이션이 있는지 확인한다
func hasSSAAnnotation(config Resource) bool {
	syncOpts := getAnnotation(config, AnnotationSyncOptions)
	return strings.Contains(syncOpts, "ServerSideApply=true")
}

// ─────────────────────────────────────────────
// Normalizer — diff 전 알려진 변동 필드 제거
// 실제: gitops-engine/pkg/diff/diff.go:64-67 Normalizer 인터페이스
// ─────────────────────────────────────────────

// Normalizer는 리소스를 정규화하는 인터페이스
type Normalizer interface {
	Normalize(r Resource) Resource
}

// KnownFieldsNormalizer는 diff 노이즈를 유발하는 알려진 필드를 제거한다.
// 실제 Argo CD는 resourceOverrides와 Lua 스크립트를 통해 이를 구성 가능하게 만든다.
type KnownFieldsNormalizer struct {
	// ignorePaths는 제거할 JSON 경로 목록
	ignorePaths [][]string
}

func NewKnownFieldsNormalizer() *KnownFieldsNormalizer {
	return &KnownFieldsNormalizer{
		ignorePaths: [][]string{
			// kubectl이 자동으로 설정하는 필드들
			{"metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration"},
			// k8s 서버가 자동으로 채우는 필드들
			{"metadata", "creationTimestamp"},
			{"metadata", "resourceVersion"},
			{"metadata", "uid"},
			{"metadata", "generation"},
			{"status"},
		},
	}
}

// Normalize는 diff 전 노이즈 필드를 제거한다
func (n *KnownFieldsNormalizer) Normalize(r Resource) Resource {
	if r == nil {
		return nil
	}
	result := clone(r)
	for _, path := range n.ignorePaths {
		removeNestedField(result, path...)
	}
	return result
}

// removeNestedField는 중첩 맵에서 특정 경로의 필드를 삭제한다
func removeNestedField(obj map[string]interface{}, fields ...string) {
	if len(fields) == 0 {
		return
	}
	if len(fields) == 1 {
		delete(obj, fields[0])
		return
	}
	nested, ok := obj[fields[0]].(map[string]interface{})
	if !ok {
		return
	}
	removeNestedField(nested, fields[1:]...)
	if len(nested) == 0 {
		delete(obj, fields[0])
	}
}

// ─────────────────────────────────────────────
// diff 모드 결정 트리
// 실제: gitops-engine/pkg/diff/diff.go:76-134 Diff() 함수
// ─────────────────────────────────────────────

// selectDiffMode는 config와 live 상태를 바탕으로 어떤 diff 알고리즘을 쓸지 결정한다.
//
// 결정 트리:
//  1. serverSideDiff 옵션 ON → ServerSideDiff (API 서버 dry-run 필요, 여기서는 미지원)
//  2. config에 ServerSideApply=true 어노테이션 → StructuredMergeDiff
//  3. live에 last-applied-configuration 어노테이션 → ThreeWayDiff
//  4. 그 외 → TwoWayDiff (fallback)
func selectDiffMode(config, live Resource, serverSideDiffEnabled bool) DiffMode {
	if serverSideDiffEnabled {
		return DiffModeServerSide
	}
	if config != nil && hasSSAAnnotation(config) {
		return DiffModeStructuredMerge
	}
	if live != nil && getLastAppliedConfig(live) != nil {
		return DiffModeThreeWay
	}
	return DiffModeTwoWay
}

// ─────────────────────────────────────────────
// ThreeWayDiff 알고리즘
// 실제: gitops-engine/pkg/diff/diff.go:689-724
// ─────────────────────────────────────────────

// ThreeWayDiff는 3-way 머지 패치를 사용한 diff를 수행한다.
//
// 입력:
//   - orig: last-applied-configuration (Argo CD가 마지막으로 적용한 상태)
//   - config: desired state (Git에서 원하는 상태)
//   - live: actual state (클러스터의 실제 상태)
//
// 알고리즘:
//  1. "Argo CD가 변경한 것": orig vs config 비교
//  2. "다른 주체가 변경한 것": orig vs live 비교
//  3. predictedLive = live + (Argo CD의 변경사항만 적용)
//     → live가 이미 반영한 것은 제외하고, 아직 반영 안 된 Argo CD 변경만 감지
func ThreeWayDiff(orig, config, live Resource, normalizer Normalizer) *DiffResult {
	// 정규화: diff 노이즈 제거
	normOrig := normalizer.Normalize(orig)
	normConfig := normalizer.Normalize(config)
	normLive := normalizer.Normalize(live)

	// 3-way 머지 패치 계산
	patch := threeWayMergePatch(normOrig, normConfig, normLive)

	// predictedLive = live에 패치 적용
	predictedLive := applyMergePatch(normLive, patch)

	// 비교: predictedLive vs normalizedLive
	normLiveBytes, _ := json.MarshalIndent(normLive, "", "  ")
	predictedBytes, _ := json.MarshalIndent(predictedLive, "", "  ")

	return &DiffResult{
		Modified:       !reflect.DeepEqual(normLive, predictedLive),
		NormalizedLive: normLiveBytes,
		PredictedLive:  predictedBytes,
	}
}

// TwoWayDiff는 config를 orig으로 사용한 ThreeWayDiff이다 (last-applied 없을 때 fallback)
// 실제: gitops-engine/pkg/diff/diff.go:514-520
// "TwoWayDiff performs a three-way diff and uses specified config as a recently applied config"
func TwoWayDiff(config, live Resource, normalizer Normalizer) *DiffResult {
	return ThreeWayDiff(config, clone(config), live, normalizer)
}

// threeWayMergePatch는 orig→config 변경을 live에 적용할 패치를 생성한다.
// 단순화된 구현: 맵 레벨에서 재귀적으로 처리
func threeWayMergePatch(orig, config, live Resource) Resource {
	patch := Resource{}

	// config에서 추가/변경된 키
	for k, configVal := range config {
		origVal := orig[k]
		liveVal := live[k]

		// 배열인 경우: strategic merge (name 키 기준 병합)
		if configArr, ok := configVal.([]interface{}); ok {
			mergedArr := strategicMergeArray(
				toInterfaceSlice(origVal),
				configArr,
				toInterfaceSlice(liveVal),
			)
			if !reflect.DeepEqual(mergedArr, liveVal) {
				patch[k] = mergedArr
			}
			continue
		}

		// 중첩 맵인 경우: 재귀
		if configMap, ok := configVal.(map[string]interface{}); ok {
			origMap, _ := origVal.(map[string]interface{})
			liveMap, _ := liveVal.(map[string]interface{})
			if origMap == nil {
				origMap = Resource{}
			}
			if liveMap == nil {
				liveMap = Resource{}
			}
			subPatch := threeWayMergePatch(origMap, configMap, liveMap)
			if len(subPatch) > 0 {
				patch[k] = subPatch
			}
			continue
		}

		// 스칼라: Argo CD가 변경했고 live가 아직 반영 안 한 것만 패치
		// 또는: Argo CD가 변경 안 했지만 live가 config와 다른 경우 (drift) — live 복원
		argoChanged := !reflect.DeepEqual(origVal, configVal)
		liveMatchesConfig := reflect.DeepEqual(liveVal, configVal)
		liveMatchesOrig := reflect.DeepEqual(liveVal, origVal)

		if argoChanged && !liveMatchesConfig {
			// Argo CD 변경분이 live에 아직 반영되지 않음 → 패치 필요
			patch[k] = configVal
		} else if !argoChanged && !liveMatchesOrig && !liveMatchesConfig {
			// Argo CD는 변경 안 했지만 외부에서 변경 → live drift
			// ThreeWayDiff: predictedLive는 config를 반영해야 하므로 패치
			patch[k] = configVal
		}
	}

	// orig에는 있었지만 config에서 제거된 키 (Argo CD가 삭제한 것)
	for k := range orig {
		if _, inConfig := config[k]; !inConfig {
			// live가 이미 삭제했으면 패치 불필요
			if _, inLive := live[k]; inLive {
				patch["$delete:"+k] = nil
			}
		}
	}

	return patch
}

// applyMergePatch는 live에 patch를 적용한다
func applyMergePatch(live, patch Resource) Resource {
	result := clone(live)
	if result == nil {
		result = Resource{}
	}
	for k, v := range patch {
		if strings.HasPrefix(k, "$delete:") {
			realKey := strings.TrimPrefix(k, "$delete:")
			delete(result, realKey)
			continue
		}
		if subPatch, ok := v.(map[string]interface{}); ok {
			existing, _ := result[k].(map[string]interface{})
			if existing == nil {
				existing = Resource{}
			}
			result[k] = applyMergePatch(existing, subPatch)
		} else {
			result[k] = v
		}
	}
	return result
}

// ─────────────────────────────────────────────
// Strategic Merge Patch — 배열 병합 (name 키 기준)
// 실제: gitops-engine/pkg/diff/diff.go:794-800에서 strategicpatch 사용
// ─────────────────────────────────────────────

// strategicMergeArray는 Kubernetes strategic merge patch 방식으로 배열을 병합한다.
// containers, volumes 등 name 키를 가진 배열은 name으로 매칭하여 병합한다.
func strategicMergeArray(orig, config, live []interface{}) []interface{} {
	// name 키가 있으면 strategic merge, 없으면 단순 config 우선
	if len(config) > 0 {
		if _, hasName := getMapKey(config[0], "name"); hasName {
			return strategicMergeByName(orig, config, live)
		}
	}
	return config
}

// strategicMergeByName은 name 필드를 키로 배열 항목을 병합한다
func strategicMergeByName(orig, config, live []interface{}) []interface{} {
	// live 상태의 name→item 인덱스
	liveByName := make(map[string]map[string]interface{})
	for _, item := range live {
		if m, ok := item.(map[string]interface{}); ok {
			if name, ok := m["name"].(string); ok {
				liveByName[name] = m
			}
		}
	}

	result := make([]interface{}, 0, len(config))
	for _, configItem := range config {
		configMap, ok := configItem.(map[string]interface{})
		if !ok {
			result = append(result, configItem)
			continue
		}
		name, _ := configMap["name"].(string)
		if liveItem, exists := liveByName[name]; exists {
			// live 항목에 config 항목 병합
			merged := clone(liveItem)
			for k, v := range configMap {
				merged[k] = v
			}
			result = append(result, merged)
		} else {
			result = append(result, configMap)
		}
	}
	return result
}

func getMapKey(v interface{}, key string) (interface{}, bool) {
	if m, ok := v.(map[string]interface{}); ok {
		val, exists := m[key]
		return val, exists
	}
	return nil, false
}

func toInterfaceSlice(v interface{}) []interface{} {
	if v == nil {
		return nil
	}
	if s, ok := v.([]interface{}); ok {
		return s
	}
	return nil
}

// ─────────────────────────────────────────────
// 메인 Diff 함수 (모드 결정 + 실행)
// 실제: gitops-engine/pkg/diff/diff.go:76-134
// ─────────────────────────────────────────────

type DiffOptions struct {
	ServerSideDiffEnabled bool
	Normalizer            Normalizer
}

// Diff는 config와 live를 비교한다. 모드는 자동 선택된다.
func Diff(config, live Resource, opts DiffOptions) (DiffMode, *DiffResult) {
	normalizer := opts.Normalizer
	if normalizer == nil {
		normalizer = NewKnownFieldsNormalizer()
	}

	mode := selectDiffMode(config, live, opts.ServerSideDiffEnabled)

	switch mode {
	case DiffModeServerSide:
		// 실제로는 k8s API 서버에 dry-run apply 후 비교. 여기서는 ThreeWayDiff로 대체.
		fmt.Println("  [ServerSideDiff] k8s API dry-run apply (시뮬레이션: ThreeWayDiff로 대체)")
		orig := getLastAppliedConfig(live)
		if orig == nil {
			orig = config
		}
		return mode, ThreeWayDiff(orig, config, live, normalizer)

	case DiffModeStructuredMerge:
		// 실제로는 structured-merge-diff 라이브러리 사용. 여기서는 ThreeWayDiff로 대체.
		fmt.Println("  [StructuredMergeDiff] SSA 기반 diff (시뮬레이션: ThreeWayDiff로 대체)")
		orig := getLastAppliedConfig(live)
		if orig == nil {
			orig = config
		}
		return mode, ThreeWayDiff(orig, config, live, normalizer)

	case DiffModeThreeWay:
		orig := getLastAppliedConfig(live)
		return mode, ThreeWayDiff(orig, config, live, normalizer)

	default: // TwoWay
		return mode, TwoWayDiff(config, live, normalizer)
	}
}

// ─────────────────────────────────────────────
// 출력 헬퍼
// ─────────────────────────────────────────────

func printDivider(title string) {
	fmt.Printf("\n%s\n%s\n", strings.Repeat("=", 60), title)
}

func printDiffResult(mode DiffMode, result *DiffResult) {
	fmt.Printf("  Diff 모드: %s\n", mode)
	fmt.Printf("  수정됨: %v\n", result.Modified)
	if result.Modified {
		normLines := strings.Split(strings.TrimSpace(string(result.NormalizedLive)), "\n")
		predLines := strings.Split(strings.TrimSpace(string(result.PredictedLive)), "\n")
		fmt.Println("\n  [NormalizedLive] (실제 클러스터 상태):")
		for _, l := range normLines {
			fmt.Println("    " + l)
		}
		fmt.Println("\n  [PredictedLive] (Git 반영 후 기대 상태):")
		for _, l := range predLines {
			fmt.Println("    " + l)
		}
		fmt.Println("\n  [변경사항 요약]:")
		printChangeSummary(result.NormalizedLive, result.PredictedLive)
	} else {
		fmt.Println("  -> 변경 없음 (Synced)")
	}
}

// printChangeSummary는 두 JSON 상태의 차이를 사람이 읽기 쉽게 출력한다
func printChangeSummary(liveBytes, predBytes []byte) {
	var live, pred map[string]interface{}
	_ = json.Unmarshal(liveBytes, &live)
	_ = json.Unmarshal(predBytes, &pred)
	diffs := findDiffs("", live, pred)
	sort.Strings(diffs)
	for _, d := range diffs {
		fmt.Println("    " + d)
	}
}

func findDiffs(prefix string, live, pred map[string]interface{}) []string {
	var result []string
	allKeys := make(map[string]bool)
	for k := range live {
		allKeys[k] = true
	}
	for k := range pred {
		allKeys[k] = true
	}
	for k := range allKeys {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		lv := live[k]
		pv := pred[k]
		if reflect.DeepEqual(lv, pv) {
			continue
		}
		lm, lok := lv.(map[string]interface{})
		pm, pok := pv.(map[string]interface{})
		if lok && pok {
			result = append(result, findDiffs(path, lm, pm)...)
		} else {
			result = append(result, fmt.Sprintf("~ %s: %v -> %v", path, jsonStr(lv), jsonStr(pv)))
		}
	}
	return result
}

func jsonStr(v interface{}) string {
	if v == nil {
		return "<nil>"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// ─────────────────────────────────────────────
// 시나리오 데이터 헬퍼
// ─────────────────────────────────────────────

// makeDeployment는 테스트용 Deployment 리소스를 생성한다
func makeDeployment(name string, replicas int, image string, extraAnnotations map[string]string) Resource {
	annotations := map[string]interface{}{}
	for k, v := range extraAnnotations {
		annotations[k] = v
	}

	return Resource{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": "default",
			"annotations": annotations,
			"resourceVersion": "12345",
			"uid":             "abc-123",
			"generation":      float64(1),
		},
		"spec": map[string]interface{}{
			"replicas": float64(replicas),
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "app",
							"image": image,
							"resources": map[string]interface{}{
								"requests": map[string]interface{}{
									"cpu":    "100m",
									"memory": "128Mi",
								},
							},
						},
					},
				},
			},
		},
		"status": map[string]interface{}{
			"readyReplicas":     float64(replicas),
			"availableReplicas": float64(replicas),
		},
	}
}

// withLastApplied는 config를 last-applied-configuration 어노테이션으로 인코딩한다
func withLastApplied(live, lastApplied Resource) Resource {
	result := clone(live)
	b, _ := json.Marshal(lastApplied)
	meta := result["metadata"].(map[string]interface{})
	annotations, ok := meta["annotations"].(map[string]interface{})
	if !ok {
		annotations = map[string]interface{}{}
	}
	annotations[AnnotationLastAppliedConfig] = string(b)
	meta["annotations"] = annotations
	return result
}

// ─────────────────────────────────────────────
// 시나리오 실행
// ─────────────────────────────────────────────

func main() {
	fmt.Println("=== Argo CD GitOps Diff 시스템 시뮬레이션 ===")
	fmt.Println()
	fmt.Println("참조: gitops-engine/pkg/diff/diff.go")
	fmt.Println()
	fmt.Println("Diff 모드 결정 트리:")
	fmt.Println("  serverSideDiff=true  → ServerSideDiff  (API dry-run)")
	fmt.Println("  SSA 어노테이션       → StructuredMergeDiff")
	fmt.Println("  last-applied 존재    → ThreeWayDiff")
	fmt.Println("  그 외 (fallback)     → TwoWayDiff")

	normalizer := NewKnownFieldsNormalizer()

	// ─────────────────────────────────────────────
	// 시나리오 1: 변경 없음 (Synced)
	// ─────────────────────────────────────────────
	printDivider("시나리오 1: 변경 없음 (Synced)")
	fmt.Println("상황: Git 설정과 클러스터 상태가 동일")
	fmt.Println("      config = live = last-applied (완전 동일)")

	// 단순한 리소스로 테스트 (containers 배열 없이 scalar만)
	config1 := Resource{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": "myapp", "namespace": "default", "resourceVersion": "100"},
		"spec":       map[string]interface{}{"replicas": float64(3), "image": "nginx:1.21"},
		"status":     map[string]interface{}{"readyReplicas": float64(3)},
	}
	// last-applied = config1, live = config1 (동일한 상태)
	live1 := withLastApplied(clone(config1), config1)

	mode1, result1 := Diff(config1, live1, DiffOptions{Normalizer: normalizer})
	printDiffResult(mode1, result1)

	// ─────────────────────────────────────────────
	// 시나리오 2: Config 변경 (OutOfSync — Argo CD 업데이트 필요)
	// ─────────────────────────────────────────────
	printDivider("시나리오 2: Config 변경 (OutOfSync)")
	fmt.Println("상황: Git에서 replicas 3→5, 이미지 nginx:1.21→nginx:1.23으로 변경")
	fmt.Println("      live에는 아직 old config (replicas=3, nginx:1.21)")

	origConfig2 := makeDeployment("myapp", 3, "nginx:1.21", nil)
	newConfig2 := makeDeployment("myapp", 5, "nginx:1.23", nil)
	live2 := withLastApplied(makeDeployment("myapp", 3, "nginx:1.21", nil), origConfig2)

	mode2, result2 := Diff(newConfig2, live2, DiffOptions{Normalizer: normalizer})
	printDiffResult(mode2, result2)

	// ─────────────────────────────────────────────
	// 시나리오 3: Live Drift (다른 주체가 변경)
	// ─────────────────────────────────────────────
	printDivider("시나리오 3: Live Drift (외부 변경, OutOfSync)")
	fmt.Println("상황: 누군가 kubectl로 직접 replicas=1로 줄임")
	fmt.Println("      Git(config)=3, last-applied=3, live=1")
	fmt.Println("      ThreeWayDiff: Argo CD는 replicas를 변경하지 않았지만")
	fmt.Println("      live가 config와 다르므로 OutOfSync 감지")

	config3 := makeDeployment("myapp", 3, "nginx:1.21", nil)
	origConfig3 := makeDeployment("myapp", 3, "nginx:1.21", nil)
	// live: 외부에서 replicas를 1로 낮춤
	liveBase3 := makeDeployment("myapp", 1, "nginx:1.21", nil)
	live3 := withLastApplied(liveBase3, origConfig3)

	mode3, result3 := Diff(config3, live3, DiffOptions{Normalizer: normalizer})
	printDiffResult(mode3, result3)

	// ─────────────────────────────────────────────
	// 시나리오 4: 양쪽 모두 변경 (3-way merge의 핵심)
	// ─────────────────────────────────────────────
	printDivider("시나리오 4: 양쪽 변경 (ThreeWayDiff의 핵심)")
	fmt.Println("상황:")
	fmt.Println("  - Argo CD(Git): image nginx:1.21 → nginx:1.23 변경")
	fmt.Println("  - 외부(kubectl): replicas 3 → 5로 직접 변경 (HPA 등)")
	fmt.Println("")
	fmt.Println("  ThreeWayDiff 결과:")
	fmt.Println("  - Argo CD의 이미지 변경 → predictedLive에 반영 (OutOfSync)")
	fmt.Println("  - 외부의 replicas 변경 → predictedLive에 보존 (Argo CD가 손대지 않음)")

	origConfig4 := makeDeployment("myapp", 3, "nginx:1.21", nil)
	newConfig4 := makeDeployment("myapp", 3, "nginx:1.23", nil) // Argo CD: 이미지만 변경
	liveBase4 := makeDeployment("myapp", 5, "nginx:1.21", nil)  // 외부: replicas만 변경
	live4 := withLastApplied(liveBase4, origConfig4)

	mode4, result4 := Diff(newConfig4, live4, DiffOptions{Normalizer: normalizer})
	printDiffResult(mode4, result4)

	// ─────────────────────────────────────────────
	// 시나리오 5: TwoWayDiff (last-applied 없는 경우)
	// ─────────────────────────────────────────────
	printDivider("시나리오 5: TwoWayDiff (last-applied 어노테이션 없음)")
	fmt.Println("상황: Helm 등으로 설치된 리소스 — last-applied 없음")
	fmt.Println("      TwoWayDiff = config를 orig으로 사용하는 ThreeWayDiff")
	fmt.Println("      외부 변경(replicas drift)도 OutOfSync로 잡힘")

	config5 := makeDeployment("helm-app", 3, "nginx:1.21", nil)
	live5 := makeDeployment("helm-app", 5, "nginx:1.21", nil) // last-applied 없음
	// live5에 어노테이션 없음 → TwoWayDiff 모드 선택됨

	mode5, result5 := Diff(config5, live5, DiffOptions{Normalizer: normalizer})
	printDiffResult(mode5, result5)

	// ─────────────────────────────────────────────
	// 시나리오 6: Strategic Merge — containers 배열
	// ─────────────────────────────────────────────
	printDivider("시나리오 6: Strategic Merge Patch (containers 배열)")
	fmt.Println("상황: 두 컨테이너가 있고 Argo CD가 하나만 변경")

	multiContainerConfig := Resource{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": "multi"},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"name": "app", "image": "app:v2"},   // 변경
						map[string]interface{}{"name": "sidecar", "image": "proxy:v1"}, // 유지
					},
				},
			},
		},
	}

	lastApplied6 := Resource{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": "multi"},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"name": "app", "image": "app:v1"},
						map[string]interface{}{"name": "sidecar", "image": "proxy:v1"},
					},
				},
			},
		},
	}

	multiContainerLive := withLastApplied(Resource{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": "multi", "resourceVersion": "999"},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "app",
							"image": "app:v1",
							"ports": []interface{}{map[string]interface{}{"containerPort": float64(8080)}},
						},
						map[string]interface{}{
							"name":  "sidecar",
							"image": "proxy:v1",
						},
					},
				},
			},
		},
	}, lastApplied6)

	mode6, result6 := Diff(multiContainerConfig, multiContainerLive, DiffOptions{Normalizer: normalizer})
	printDiffResult(mode6, result6)
	fmt.Println("\n  -> strategic merge: app 컨테이너 이미지만 v2로 변경, ports는 live에서 보존됨")

	// 요약
	fmt.Println()
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println("Diff 시스템 핵심 포인트:")
	fmt.Println("  1. last-applied 어노테이션 → ThreeWayDiff (가장 정확)")
	fmt.Println("  2. ThreeWayDiff: Argo CD 변경 vs 외부 변경 분리")
	fmt.Println("  3. 외부 변경은 OutOfSync로 잡되, predictedLive에는 live 값 보존")
	fmt.Println("  4. TwoWayDiff: fallback, 외부 변경도 전부 OutOfSync")
	fmt.Println("  5. Normalizer: status, uid 등 volatile 필드 제거로 노이즈 방지")
}
