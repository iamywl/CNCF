// Istio 사이드카 인젝션(Sidecar Injection) 시뮬레이션
//
// 이 PoC는 Istio의 pkg/kube/inject/ 패키지에 구현된
// 사이드카 인젝션 메커니즘을 시뮬레이션합니다.
//
// 핵심 로직:
//   1. injectRequired(): 인젝션 정책 평가 (annotation → label → namespace)
//   2. RunTemplate(): 사이드카 템플릿 렌더링 (istio-proxy, istio-init 추가)
//   3. JSON Patch 생성: 원본 Pod와 수정된 Pod를 비교하여 패치 생성
//
// 참조:
//   - istio/pkg/kube/inject/inject.go (InjectionPolicy, injectRequired, RunTemplate)
//   - istio/pkg/kube/inject/webhook.go (Webhook 처리, JSON Patch 생성)

package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ========================================================================
// Pod 스펙 모델 (Kubernetes 데이터 구조 시뮬레이션)
// ========================================================================

// ContainerPort는 컨테이너 포트 정의
type ContainerPort struct {
	Name          string `json:"name,omitempty"`
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol,omitempty"`
}

// VolumeMount는 볼륨 마운트 정의
type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// EnvVar는 환경 변수 정의
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

// SecurityContext는 보안 컨텍스트 정의
type SecurityContext struct {
	RunAsUser    *int64 `json:"runAsUser,omitempty"`
	RunAsGroup   *int64 `json:"runAsGroup,omitempty"`
	RunAsNonRoot *bool  `json:"runAsNonRoot,omitempty"`
	Privileged   *bool  `json:"privileged,omitempty"`
	Capabilities *struct {
		Add  []string `json:"add,omitempty"`
		Drop []string `json:"drop,omitempty"`
	} `json:"capabilities,omitempty"`
}

// Container는 컨테이너 정의
type Container struct {
	Name            string            `json:"name"`
	Image           string            `json:"image"`
	Ports           []ContainerPort   `json:"ports,omitempty"`
	Env             []EnvVar          `json:"env,omitempty"`
	VolumeMounts    []VolumeMount     `json:"volumeMounts,omitempty"`
	Args            []string          `json:"args,omitempty"`
	SecurityContext *SecurityContext   `json:"securityContext,omitempty"`
	ImagePullPolicy string            `json:"imagePullPolicy,omitempty"`
}

// Volume은 볼륨 정의
type Volume struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // emptyDir, secret, configMap, projected 등
	Source   string `json:"source,omitempty"`
}

// ObjectMeta는 오브젝트 메타데이터
type ObjectMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// PodSpec은 Pod 스펙
type PodSpec struct {
	Containers     []Container `json:"containers"`
	InitContainers []Container `json:"initContainers,omitempty"`
	Volumes        []Volume    `json:"volumes,omitempty"`
	HostNetwork    bool        `json:"hostNetwork,omitempty"`
}

// Pod는 Kubernetes Pod
type Pod struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       PodSpec    `json:"spec"`
}

// ========================================================================
// 인젝션 정책 (Injection Policy)
// ========================================================================

// InjectionPolicy는 사이드카 인젝션 정책
// 참조: inject.go 라인 59-74
type InjectionPolicy string

const (
	InjectionPolicyDisabled InjectionPolicy = "disabled"
	InjectionPolicyEnabled  InjectionPolicy = "enabled"
)

// InjectionConfig는 인젝션 설정
// 참조: inject.go의 Config 구조체
type InjectionConfig struct {
	Policy             InjectionPolicy
	NeverInjectSelector  []map[string]string // 레이블 셀렉터: 이 레이블이 있으면 절대 인젝션 안 함
	AlwaysInjectSelector []map[string]string // 레이블 셀렉터: 이 레이블이 있으면 항상 인젝션
}

// SidecarInjectionStatus는 인젝션 결과 상태
// 참조: inject.go 라인 897-903
type SidecarInjectionStatus struct {
	InitContainers []string `json:"initContainers"`
	Containers     []string `json:"containers"`
	Volumes        []string `json:"volumes"`
	Revision       string   `json:"revision"`
}

// IgnoredNamespaces는 인젝션을 건너뛸 네임스페이스
var IgnoredNamespaces = map[string]bool{
	"kube-system":  true,
	"kube-public":  true,
	"kube-node-lease": true,
	"istio-system": true,
}

// ========================================================================
// injectRequired - 인젝션 필요 여부 판단
// ========================================================================

// injectRequired는 Pod에 사이드카 인젝션이 필요한지 결정
// 참조: inject.go 라인 199-315
//
// 우선순위 (높은 순):
// 1. hostNetwork == true → 항상 스킵
// 2. 무시 네임스페이스 (kube-system 등) → 항상 스킵
// 3. Pod 레이블 sidecar.istio.io/inject → true/false
// 4. Pod 어노테이션 sidecar.istio.io/inject → true/false (레이블이 우선)
// 5. NeverInjectSelector 매칭 → false
// 6. AlwaysInjectSelector 매칭 → true
// 7. 네임스페이스 정책 (config.Policy) → enabled면 true, disabled면 false
func injectRequired(config *InjectionConfig, pod *Pod, namespaceLabels map[string]string) (bool, string) {
	metadata := pod.Metadata
	podSpec := pod.Spec

	// 1. hostNetwork 체크
	if podSpec.HostNetwork {
		return false, "hostNetwork이 활성화되어 스킵"
	}

	// 2. 무시 네임스페이스 체크
	if IgnoredNamespaces[metadata.Namespace] {
		return false, fmt.Sprintf("네임스페이스 %q는 무시 대상", metadata.Namespace)
	}

	var useDefault bool
	var inject bool

	// 3-4. 어노테이션/레이블 확인
	// 실제 코드: 레이블이 있으면 어노테이션보다 우선
	objectSelector := ""
	if ann, ok := metadata.Annotations["sidecar.istio.io/inject"]; ok {
		objectSelector = ann
	}
	// 레이블이 어노테이션을 오버라이드 (신규 API가 레이블)
	if lbl, ok := metadata.Labels["sidecar.istio.io/inject"]; ok {
		objectSelector = lbl
	}

	switch objectSelector {
	case "true":
		inject = true
	case "false":
		inject = false
	case "":
		useDefault = true
	default:
		useDefault = true
	}

	// 5. NeverInjectSelector 확인
	if useDefault {
		for _, selector := range config.NeverInjectSelector {
			if labelsMatch(metadata.Labels, selector) {
				inject = false
				useDefault = false
				reason := fmt.Sprintf("NeverInjectSelector 매칭: %v", selector)
				_ = reason
				break
			}
		}
	}

	// 6. AlwaysInjectSelector 확인
	if useDefault {
		for _, selector := range config.AlwaysInjectSelector {
			if labelsMatch(metadata.Labels, selector) {
				inject = true
				useDefault = false
				break
			}
		}
	}

	// 7. 기본 정책 적용
	var required bool
	var reason string
	switch config.Policy {
	case InjectionPolicyDisabled:
		if useDefault {
			required = false
			reason = "정책=disabled, 기본값 사용 → 인젝션 안 함"
		} else {
			required = inject
			if inject {
				reason = "정책=disabled이지만 명시적으로 inject=true"
			} else {
				reason = "정책=disabled, 명시적으로 inject=false"
			}
		}
	case InjectionPolicyEnabled:
		if useDefault {
			required = true
			reason = "정책=enabled, 기본값 사용 → 인젝션 수행"
		} else {
			required = inject
			if inject {
				reason = "정책=enabled, 명시적으로 inject=true"
			} else {
				reason = "정책=enabled이지만 명시적으로 inject=false"
			}
		}
	}

	return required, reason
}

// labelsMatch는 Pod 레이블이 셀렉터와 매칭되는지 확인
func labelsMatch(podLabels, selector map[string]string) bool {
	for k, v := range selector {
		if podLabels[k] != v {
			return false
		}
	}
	return true
}

// ========================================================================
// 사이드카 인젝션 템플릿 렌더링
// ========================================================================

// injectSidecar는 Pod에 사이드카 컴포넌트를 추가
// 참조: inject.go의 RunTemplate() 및 webhook.go의 inject()
func injectSidecar(pod *Pod, revision string) *Pod {
	// 원본 복사
	injected := *pod
	injected.Spec.Containers = make([]Container, len(pod.Spec.Containers))
	copy(injected.Spec.Containers, pod.Spec.Containers)
	injected.Spec.InitContainers = make([]Container, len(pod.Spec.InitContainers))
	copy(injected.Spec.InitContainers, pod.Spec.InitContainers)
	injected.Spec.Volumes = make([]Volume, len(pod.Spec.Volumes))
	copy(injected.Spec.Volumes, pod.Spec.Volumes)

	// 메타데이터 복사
	injected.Metadata.Labels = copyMap(pod.Metadata.Labels)
	injected.Metadata.Annotations = copyMap(pod.Metadata.Annotations)

	proxyUID := int64(1337)
	proxyGID := int64(1337)
	nonRoot := true
	privileged := false

	// 1. istio-init 컨테이너 추가 (iptables 규칙 설정)
	// 참조: istio/tools/istio-iptables
	initContainer := Container{
		Name:  "istio-init",
		Image: "docker.io/istio/proxyv2:1.20.0",
		Args: []string{
			"istio-iptables",
			"-p", "15001",      // ENVOY_PORT
			"-z", "15006",      // INBOUND_CAPTURE_PORT
			"-u", "1337",       // PROXY_UID
			"-m", "REDIRECT",   // REDIRECT_MODE
			"-i", "*",          // ISTIO_INBOUND_INTERCEPTION_MODE
			"-x", "",           // EXCLUDE_INTERFACES
			"-b", "*",          // ISTIO_INBOUND_PORTS
			"-d", "15090,15021,15020", // EXCLUDE_INBOUND_PORTS
		},
		SecurityContext: &SecurityContext{
			Capabilities: &struct {
				Add  []string `json:"add,omitempty"`
				Drop []string `json:"drop,omitempty"`
			}{
				Add:  []string{"NET_ADMIN", "NET_RAW"},
				Drop: []string{"ALL"},
			},
			Privileged: &privileged,
			RunAsUser:  &proxyUID,
			RunAsGroup: &proxyGID,
			RunAsNonRoot: &nonRoot,
		},
		ImagePullPolicy: "IfNotPresent",
	}
	injected.Spec.InitContainers = append(injected.Spec.InitContainers, initContainer)

	// 2. istio-proxy 사이드카 컨테이너 추가
	// 참조: manifests/charts/istio-control/istio-discovery/templates/
	sidecarContainer := Container{
		Name:  "istio-proxy",
		Image: "docker.io/istio/proxyv2:1.20.0",
		Ports: []ContainerPort{
			{Name: "http-envoy-prom", ContainerPort: 15090, Protocol: "TCP"},
		},
		Args: []string{
			"proxy", "sidecar",
			"--domain", pod.Metadata.Namespace + ".svc.cluster.local",
			"--proxyLogLevel", "warning",
			"--proxyComponentLogLevel", "misc:error",
			"--log_output_level", "default:info",
		},
		Env: []EnvVar{
			{Name: "JWT_POLICY", Value: "third-party-jwt"},
			{Name: "PILOT_CERT_PROVIDER", Value: "istiod"},
			{Name: "CA_ADDR", Value: "istiod.istio-system.svc:15012"},
			{Name: "POD_NAME", Value: pod.Metadata.Name},
			{Name: "POD_NAMESPACE", Value: pod.Metadata.Namespace},
			{Name: "INSTANCE_IP"},
			{Name: "SERVICE_ACCOUNT"},
			{Name: "HOST_IP"},
			{Name: "ISTIO_META_POD_PORTS"},
			{Name: "ISTIO_META_APP_CONTAINERS", Value: getAppContainerNames(pod)},
			{Name: "ISTIO_META_CLUSTER_ID", Value: "Kubernetes"},
			{Name: "ISTIO_META_NODE_NAME"},
			{Name: "ISTIO_META_INTERCEPTION_MODE", Value: "REDIRECT"},
		},
		VolumeMounts: []VolumeMount{
			{Name: "workload-socket", MountPath: "/var/run/secrets/workload-spiffe-uds"},
			{Name: "istio-envoy", MountPath: "/etc/istio/proxy"},
			{Name: "credential-socket", MountPath: "/var/run/secrets/credential-uds"},
			{Name: "istiod-ca-cert", MountPath: "/var/run/secrets/istio"},
			{Name: "istio-data", MountPath: "/var/lib/istio/data"},
			{Name: "istio-podinfo", MountPath: "/etc/istio/pod"},
		},
		SecurityContext: &SecurityContext{
			RunAsUser:    &proxyUID,
			RunAsGroup:   &proxyGID,
			RunAsNonRoot: &nonRoot,
			Privileged:   &privileged,
			Capabilities: &struct {
				Add  []string `json:"add,omitempty"`
				Drop []string `json:"drop,omitempty"`
			}{
				Drop: []string{"ALL"},
			},
		},
		ImagePullPolicy: "IfNotPresent",
	}
	injected.Spec.Containers = append(injected.Spec.Containers, sidecarContainer)

	// 3. 볼륨 추가
	volumes := []Volume{
		{Name: "workload-socket", Type: "emptyDir"},
		{Name: "credential-socket", Type: "emptyDir"},
		{Name: "workload-certs", Type: "emptyDir"},
		{Name: "istio-envoy", Type: "emptyDir"},
		{Name: "istio-data", Type: "emptyDir"},
		{Name: "istiod-ca-cert", Type: "configMap", Source: "istio-ca-root-cert"},
		{Name: "istio-podinfo", Type: "downwardAPI"},
	}
	injected.Spec.Volumes = append(injected.Spec.Volumes, volumes...)

	// 4. 어노테이션/레이블 추가
	if injected.Metadata.Labels == nil {
		injected.Metadata.Labels = make(map[string]string)
	}
	injected.Metadata.Labels["security.istio.io/tlsMode"] = "istio"
	injected.Metadata.Labels["service.istio.io/canonical-name"] = pod.Metadata.Name
	injected.Metadata.Labels["service.istio.io/canonical-revision"] = "latest"

	if injected.Metadata.Annotations == nil {
		injected.Metadata.Annotations = make(map[string]string)
	}

	// 인젝션 상태 어노테이션
	status := SidecarInjectionStatus{
		InitContainers: []string{"istio-init"},
		Containers:     []string{"istio-proxy"},
		Volumes:        []string{"workload-socket", "credential-socket", "workload-certs", "istio-envoy", "istio-data", "istiod-ca-cert", "istio-podinfo"},
		Revision:       revision,
	}
	statusBytes, _ := json.Marshal(status)
	injected.Metadata.Annotations["sidecar.istio.io/status"] = string(statusBytes)
	injected.Metadata.Annotations["istio.io/rev"] = revision

	return &injected
}

func getAppContainerNames(pod *Pod) string {
	names := make([]string, len(pod.Spec.Containers))
	for i, c := range pod.Spec.Containers {
		names[i] = c.Name
	}
	return strings.Join(names, ",")
}

func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

// ========================================================================
// JSON Patch 생성
// ========================================================================

// JSONPatchOp는 RFC 6902 JSON Patch 연산
type JSONPatchOp struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// generateJSONPatch는 원본 Pod와 수정된 Pod를 비교하여 JSON Patch를 생성
// 참조: webhook.go의 createPatch() 함수
func generateJSONPatch(original, modified *Pod) []JSONPatchOp {
	var patches []JSONPatchOp

	// Init 컨테이너 패치
	if len(modified.Spec.InitContainers) > len(original.Spec.InitContainers) {
		for i := len(original.Spec.InitContainers); i < len(modified.Spec.InitContainers); i++ {
			if len(original.Spec.InitContainers) == 0 && i == len(original.Spec.InitContainers) {
				// initContainers 배열이 없으면 추가
				patches = append(patches, JSONPatchOp{
					Op:    "add",
					Path:  "/spec/initContainers",
					Value: []Container{modified.Spec.InitContainers[i]},
				})
			} else {
				patches = append(patches, JSONPatchOp{
					Op:    "add",
					Path:  fmt.Sprintf("/spec/initContainers/%d", i),
					Value: modified.Spec.InitContainers[i],
				})
			}
		}
	}

	// 사이드카 컨테이너 패치
	if len(modified.Spec.Containers) > len(original.Spec.Containers) {
		for i := len(original.Spec.Containers); i < len(modified.Spec.Containers); i++ {
			patches = append(patches, JSONPatchOp{
				Op:    "add",
				Path:  fmt.Sprintf("/spec/containers/%d", i),
				Value: modified.Spec.Containers[i],
			})
		}
	}

	// 볼륨 패치
	if len(modified.Spec.Volumes) > len(original.Spec.Volumes) {
		for i := len(original.Spec.Volumes); i < len(modified.Spec.Volumes); i++ {
			if len(original.Spec.Volumes) == 0 && i == len(original.Spec.Volumes) {
				patches = append(patches, JSONPatchOp{
					Op:    "add",
					Path:  "/spec/volumes",
					Value: []Volume{modified.Spec.Volumes[i]},
				})
			} else {
				patches = append(patches, JSONPatchOp{
					Op:    "add",
					Path:  fmt.Sprintf("/spec/volumes/%d", i),
					Value: modified.Spec.Volumes[i],
				})
			}
		}
	}

	// 레이블 패치
	if modified.Metadata.Labels != nil {
		for k, v := range modified.Metadata.Labels {
			if original.Metadata.Labels == nil || original.Metadata.Labels[k] != v {
				if original.Metadata.Labels == nil {
					patches = append(patches, JSONPatchOp{
						Op:    "add",
						Path:  "/metadata/labels",
						Value: map[string]string{k: v},
					})
					break // 한 번에 추가
				}
				escapedKey := strings.ReplaceAll(k, "/", "~1")
				patches = append(patches, JSONPatchOp{
					Op:    "add",
					Path:  fmt.Sprintf("/metadata/labels/%s", escapedKey),
					Value: v,
				})
			}
		}
	}

	// 어노테이션 패치
	if modified.Metadata.Annotations != nil {
		for k, v := range modified.Metadata.Annotations {
			if original.Metadata.Annotations == nil || original.Metadata.Annotations[k] != v {
				if original.Metadata.Annotations == nil {
					patches = append(patches, JSONPatchOp{
						Op:    "add",
						Path:  "/metadata/annotations",
						Value: modified.Metadata.Annotations,
					})
					break // 한 번에 추가
				}
				escapedKey := strings.ReplaceAll(k, "/", "~1")
				patches = append(patches, JSONPatchOp{
					Op:    "add",
					Path:  fmt.Sprintf("/metadata/annotations/%s", escapedKey),
					Value: v,
				})
			}
		}
	}

	return patches
}

// ========================================================================
// 출력 헬퍼 함수
// ========================================================================

func printPodSummary(label string, pod *Pod) {
	fmt.Printf("\n%s:\n", label)
	fmt.Println(strings.Repeat("-", 50))
	fmt.Printf("  이름: %s/%s\n", pod.Metadata.Namespace, pod.Metadata.Name)

	if len(pod.Metadata.Labels) > 0 {
		fmt.Println("  레이블:")
		for k, v := range pod.Metadata.Labels {
			fmt.Printf("    %s: %s\n", k, v)
		}
	}

	if len(pod.Metadata.Annotations) > 0 {
		fmt.Println("  어노테이션:")
		for k, v := range pod.Metadata.Annotations {
			if k == "sidecar.istio.io/status" {
				fmt.Printf("    %s: <인젝션 상태 JSON>\n", k)
			} else {
				fmt.Printf("    %s: %s\n", k, v)
			}
		}
	}

	if len(pod.Spec.InitContainers) > 0 {
		fmt.Println("  Init 컨테이너:")
		for _, c := range pod.Spec.InitContainers {
			fmt.Printf("    - %s (이미지: %s)\n", c.Name, c.Image)
		}
	}

	fmt.Println("  컨테이너:")
	for _, c := range pod.Spec.Containers {
		fmt.Printf("    - %s (이미지: %s)\n", c.Name, c.Image)
		if len(c.Ports) > 0 {
			for _, p := range c.Ports {
				fmt.Printf("      포트: %d/%s\n", p.ContainerPort, p.Protocol)
			}
		}
	}

	if len(pod.Spec.Volumes) > 0 {
		fmt.Println("  볼륨:")
		for _, v := range pod.Spec.Volumes {
			if v.Source != "" {
				fmt.Printf("    - %s (%s: %s)\n", v.Name, v.Type, v.Source)
			} else {
				fmt.Printf("    - %s (%s)\n", v.Name, v.Type)
			}
		}
	}
}

func printJSON(label string, v interface{}) {
	data, _ := json.MarshalIndent(v, "  ", "  ")
	fmt.Printf("\n%s:\n  %s\n", label, string(data))
}

// ========================================================================
// 시뮬레이션 실행
// ========================================================================

func main() {
	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println("  Istio 사이드카 인젝션(Sidecar Injection) 시뮬레이션")
	fmt.Println("  참조: istio/pkg/kube/inject/inject.go, webhook.go")
	fmt.Println("=" + strings.Repeat("=", 79))

	config := &InjectionConfig{
		Policy: InjectionPolicyEnabled,
		NeverInjectSelector: []map[string]string{
			{"sidecar.istio.io/inject": "false"},
		},
		AlwaysInjectSelector: []map[string]string{
			{"istio-injection": "enabled"},
		},
	}

	// =================================================================
	// 1단계: 인젝션 정책 평가 (injectRequired)
	// =================================================================
	fmt.Println()
	fmt.Println("[1단계] 인젝션 정책 평가 (injectRequired)")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println()
	fmt.Println("우선순위: hostNetwork → 무시 NS → Label → Annotation → NeverInjectSelector → AlwaysInjectSelector → NS 정책")
	fmt.Println()

	testCases := []struct {
		name            string
		pod             *Pod
		namespaceLabels map[string]string
	}{
		{
			name: "1. hostNetwork=true",
			pod: &Pod{
				Metadata: ObjectMeta{Name: "host-net-pod", Namespace: "default"},
				Spec:     PodSpec{HostNetwork: true, Containers: []Container{{Name: "app", Image: "app:v1"}}},
			},
		},
		{
			name: "2. kube-system 네임스페이스",
			pod: &Pod{
				Metadata: ObjectMeta{Name: "kube-dns", Namespace: "kube-system"},
				Spec:     PodSpec{Containers: []Container{{Name: "dns", Image: "dns:v1"}}},
			},
		},
		{
			name: "3. Label inject=true (정책=enabled)",
			pod: &Pod{
				Metadata: ObjectMeta{
					Name: "my-app", Namespace: "default",
					Labels: map[string]string{"sidecar.istio.io/inject": "true"},
				},
				Spec: PodSpec{Containers: []Container{{Name: "app", Image: "app:v1"}}},
			},
		},
		{
			name: "4. Label inject=false (정책=enabled)",
			pod: &Pod{
				Metadata: ObjectMeta{
					Name: "skip-app", Namespace: "default",
					Labels: map[string]string{"sidecar.istio.io/inject": "false"},
				},
				Spec: PodSpec{Containers: []Container{{Name: "app", Image: "app:v1"}}},
			},
		},
		{
			name: "5. Annotation inject=true, Label inject=false (Label 우선)",
			pod: &Pod{
				Metadata: ObjectMeta{
					Name: "conflict-app", Namespace: "default",
					Labels:      map[string]string{"sidecar.istio.io/inject": "false"},
					Annotations: map[string]string{"sidecar.istio.io/inject": "true"},
				},
				Spec: PodSpec{Containers: []Container{{Name: "app", Image: "app:v1"}}},
			},
		},
		{
			name: "6. 기본값 사용 (정책=enabled → 인젝션)",
			pod: &Pod{
				Metadata: ObjectMeta{Name: "default-app", Namespace: "default"},
				Spec:     PodSpec{Containers: []Container{{Name: "app", Image: "app:v1"}}},
			},
		},
		{
			name: "7. 기본값 + 정책=disabled",
			pod: &Pod{
				Metadata: ObjectMeta{Name: "no-inject-app", Namespace: "default"},
				Spec:     PodSpec{Containers: []Container{{Name: "app", Image: "app:v1"}}},
			},
		},
	}

	for _, tc := range testCases {
		testConfig := config
		if tc.name == "7. 기본값 + 정책=disabled" {
			testConfig = &InjectionConfig{Policy: InjectionPolicyDisabled}
		}
		required, reason := injectRequired(testConfig, tc.pod, tc.namespaceLabels)
		symbol := "X"
		if required {
			symbol = "O"
		}
		fmt.Printf("  [%s] %-50s → %s\n", symbol, tc.name, reason)
	}

	// =================================================================
	// 2단계: 사이드카 인젝션 실행
	// =================================================================
	fmt.Println()
	fmt.Println("[2단계] 사이드카 인젝션 실행")
	fmt.Println(strings.Repeat("-", 60))

	// 원본 Pod 정의
	originalPod := &Pod{
		APIVersion: "v1",
		Kind:       "Pod",
		Metadata: ObjectMeta{
			Name:      "productpage-v1-6b746f74dc-x7k9n",
			Namespace: "bookinfo",
			Labels: map[string]string{
				"app":     "productpage",
				"version": "v1",
			},
			Annotations: map[string]string{
				"kubectl.kubernetes.io/restartedAt": "2024-01-15T10:30:00Z",
			},
		},
		Spec: PodSpec{
			Containers: []Container{
				{
					Name:  "productpage",
					Image: "docker.io/istio/examples-bookinfo-productpage-v1:1.17.0",
					Ports: []ContainerPort{
						{ContainerPort: 9080, Protocol: "TCP"},
					},
					VolumeMounts: []VolumeMount{
						{Name: "tmp", MountPath: "/tmp"},
					},
				},
			},
			Volumes: []Volume{
				{Name: "tmp", Type: "emptyDir"},
			},
		},
	}

	// 원본 출력
	printPodSummary("원본 Pod (인젝션 전)", originalPod)

	// 인젝션 실행
	injectedPod := injectSidecar(originalPod, "default")

	// 인젝션 후 출력
	printPodSummary("수정된 Pod (인젝션 후)", injectedPod)

	// =================================================================
	// 3단계: JSON Patch 생성
	// =================================================================
	fmt.Println()
	fmt.Println("[3단계] JSON Patch 생성 (원본 vs 수정된 Pod)")
	fmt.Println(strings.Repeat("-", 60))

	patches := generateJSONPatch(originalPod, injectedPod)
	fmt.Printf("\n생성된 JSON Patch 연산 (%d개):\n", len(patches))

	for i, p := range patches {
		valueStr := ""
		switch v := p.Value.(type) {
		case Container:
			valueStr = fmt.Sprintf("{name: %q, image: %q}", v.Name, v.Image)
		case []Container:
			names := make([]string, len(v))
			for j, c := range v {
				names[j] = c.Name
			}
			valueStr = fmt.Sprintf("[%s]", strings.Join(names, ", "))
		case Volume:
			valueStr = fmt.Sprintf("{name: %q, type: %q}", v.Name, v.Type)
		case []Volume:
			names := make([]string, len(v))
			for j, vol := range v {
				names[j] = vol.Name
			}
			valueStr = fmt.Sprintf("[%s]", strings.Join(names, ", "))
		case string:
			if len(v) > 60 {
				valueStr = v[:57] + "..."
			} else {
				valueStr = v
			}
		case map[string]string:
			parts := make([]string, 0)
			for mk, mv := range v {
				if len(mv) > 40 {
					mv = mv[:37] + "..."
				}
				parts = append(parts, fmt.Sprintf("%s=%s", mk, mv))
			}
			valueStr = fmt.Sprintf("{%s}", strings.Join(parts, ", "))
		default:
			data, _ := json.Marshal(v)
			s := string(data)
			if len(s) > 60 {
				s = s[:57] + "..."
			}
			valueStr = s
		}

		fmt.Printf("  [%d] op=%s path=%s\n      value=%s\n", i+1, p.Op, p.Path, valueStr)
	}

	// =================================================================
	// 4단계: 전체 JSON Patch 출력
	// =================================================================
	fmt.Println()
	fmt.Println("[4단계] 전체 JSON Patch (요약)")
	fmt.Println(strings.Repeat("-", 60))

	// 간소화된 패치 출력
	simplifiedPatches := make([]map[string]interface{}, 0, len(patches))
	for _, p := range patches {
		sp := map[string]interface{}{
			"op":   p.Op,
			"path": p.Path,
		}
		switch v := p.Value.(type) {
		case Container:
			sp["value"] = fmt.Sprintf("<container: %s>", v.Name)
		case []Container:
			names := make([]string, len(v))
			for j, c := range v {
				names[j] = c.Name
			}
			sp["value"] = fmt.Sprintf("<containers: [%s]>", strings.Join(names, ", "))
		case Volume:
			sp["value"] = fmt.Sprintf("<volume: %s>", v.Name)
		case []Volume:
			names := make([]string, len(v))
			for j, vol := range v {
				names[j] = vol.Name
			}
			sp["value"] = fmt.Sprintf("<volumes: [%s]>", strings.Join(names, ", "))
		default:
			data, _ := json.Marshal(v)
			s := string(data)
			if len(s) > 80 {
				s = s[:77] + "..."
			}
			sp["value"] = s
		}
		simplifiedPatches = append(simplifiedPatches, sp)
	}
	printJSON("JSON Patch 요약", simplifiedPatches)

	// =================================================================
	// 5단계: 인젝션 흐름 요약
	// =================================================================
	fmt.Println()
	fmt.Println("[5단계] 사이드카 인젝션 흐름 요약")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println()
	fmt.Println("  Pod 생성 요청")
	fmt.Println("    │")
	fmt.Println("    ▼")
	fmt.Println("  MutatingWebhookConfiguration")
	fmt.Println("    │  (istio-sidecar-injector)")
	fmt.Println("    ▼")
	fmt.Println("  Webhook.inject()")
	fmt.Println("    │")
	fmt.Println("    ├── 1. injectRequired() 평가")
	fmt.Println("    │      ├── hostNetwork 체크")
	fmt.Println("    │      ├── 무시 네임스페이스 체크")
	fmt.Println("    │      ├── Pod label/annotation 체크")
	fmt.Println("    │      ├── NeverInject/AlwaysInject 셀렉터 체크")
	fmt.Println("    │      └── 네임스페이스 정책(enabled/disabled) 체크")
	fmt.Println("    │")
	fmt.Println("    ├── 2. RunTemplate() 실행")
	fmt.Println("    │      ├── stripPod() - 기존 인젝션 제거 (재인젝션 지원)")
	fmt.Println("    │      ├── SidecarTemplateData 생성")
	fmt.Println("    │      ├── 템플릿 렌더링 (istio-proxy, istio-init)")
	fmt.Println("    │      └── Strategic Merge Patch 적용")
	fmt.Println("    │")
	fmt.Println("    ├── 3. JSON Patch 생성")
	fmt.Println("    │      ├── 원본 Pod JSON 직렬화")
	fmt.Println("    │      ├── 수정된 Pod JSON 직렬화")
	fmt.Println("    │      └── RFC 6902 JSON Patch 계산")
	fmt.Println("    │")
	fmt.Println("    └── 4. AdmissionResponse 반환")
	fmt.Println("           ├── Allowed: true")
	fmt.Println("           ├── PatchType: JSONPatch")
	fmt.Println("           └── Patch: [JSON Patch 배열]")
	fmt.Println()

	fmt.Println("인젝션으로 추가되는 주요 구성요소:")
	fmt.Println()
	fmt.Println("  ┌──────────────────────────────────────────────┐")
	fmt.Println("  │ Pod                                          │")
	fmt.Println("  │                                              │")
	fmt.Println("  │  ┌─────────────────┐  ┌─────────────────┐   │")
	fmt.Println("  │  │  istio-init      │  │  app container  │   │")
	fmt.Println("  │  │  (init)          │  │  (원본)          │   │")
	fmt.Println("  │  │  iptables 설정    │  │                 │   │")
	fmt.Println("  │  └─────────────────┘  └─────────────────┘   │")
	fmt.Println("  │                                              │")
	fmt.Println("  │  ┌─────────────────┐                        │")
	fmt.Println("  │  │  istio-proxy     │  ← 사이드카 추가       │")
	fmt.Println("  │  │  (Envoy)         │                        │")
	fmt.Println("  │  │  포트: 15090      │                        │")
	fmt.Println("  │  └─────────────────┘                        │")
	fmt.Println("  │                                              │")
	fmt.Println("  │  Volumes: istio-envoy, istiod-ca-cert, ...  │")
	fmt.Println("  └──────────────────────────────────────────────┘")
	fmt.Println()

	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println("  시뮬레이션 완료")
	fmt.Println("=" + strings.Repeat("=", 79))
}
