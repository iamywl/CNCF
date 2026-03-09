/*
Pod Security Admission (PSA) 핵심 알고리즘 시뮬레이션

Kubernetes의 Pod Security Standards 3단계 검사 엔진을 Go 표준 라이브러리만으로 구현한다.
실제 소스코드(staging/src/k8s.io/pod-security-admission/)의 핵심 설계를 재현:

1. Level 정의: Privileged / Baseline / Restricted
2. Check Registry: 버전별 체크 함수를 사전 계산하여 맵에 저장
3. 오버라이드 시스템: Restricted 체크가 Baseline 체크를 대체
4. Namespace 라벨 기반 Policy 해석
5. Enforce / Audit / Warn 모드 평가
6. Exemption 시스템: Namespace, User, RuntimeClass 면제
7. 결과 캐싱: 동일 LevelVersion이면 평가 재사용

Usage: go run main.go
*/
package main

import (
	"fmt"
	"sort"
	"strings"
)

// =============================================================================
// 1. 기본 타입 정의 (api/constants.go, api/helpers.go 대응)
// =============================================================================

// Level은 PSS 보안 레벨이다.
type Level string

const (
	LevelPrivileged Level = "privileged"
	LevelBaseline   Level = "baseline"
	LevelRestricted Level = "restricted"
)

// Version은 major.minor 버전을 나타낸다.
type Version struct {
	Major int
	Minor int
}

func (v Version) String() string {
	return fmt.Sprintf("v%d.%d", v.Major, v.Minor)
}

func (v Version) Older(other Version) bool {
	if v.Major != other.Major {
		return v.Major < other.Major
	}
	return v.Minor < other.Minor
}

func nextMinor(v Version) Version {
	return Version{Major: v.Major, Minor: v.Minor + 1}
}

// LevelVersion은 레벨과 해당 버전의 쌍이다.
type LevelVersion struct {
	Level   Level
	Version Version
}

func (lv LevelVersion) String() string {
	return fmt.Sprintf("%s:%s", lv.Level, lv.Version)
}

// Policy는 세 모드(Enforce, Audit, Warn)의 정책을 집약한다.
type Policy struct {
	Enforce LevelVersion
	Audit   LevelVersion
	Warn    LevelVersion
}

func (p Policy) FullyPrivileged() bool {
	return p.Enforce.Level == LevelPrivileged &&
		p.Audit.Level == LevelPrivileged &&
		p.Warn.Level == LevelPrivileged
}

// =============================================================================
// 2. Pod / Container 시뮬레이션 타입
// =============================================================================

// SecurityContext는 컨테이너의 보안 설정을 시뮬레이션한다.
type SecurityContext struct {
	Privileged               *bool
	AllowPrivilegeEscalation *bool
	RunAsNonRoot             *bool
	RunAsUser                *int64
	Capabilities             *Capabilities
	ProcMount                *string
	SeccompProfile           *string // "RuntimeDefault", "Localhost", "Unconfined"
	AppArmorProfile          *string // "RuntimeDefault", "Localhost", "Unconfined"
}

type Capabilities struct {
	Add  []string
	Drop []string
}

type ContainerPort struct {
	HostPort int32
}

type Container struct {
	Name            string
	SecurityContext *SecurityContext
	Ports           []ContainerPort
}

type Volume struct {
	Name     string
	Type     string // "configMap", "secret", "hostPath", "nfs", "emptyDir", etc.
}

type PodSecurityContext struct {
	RunAsNonRoot   *bool
	RunAsUser      *int64
	SeccompProfile *string
	Sysctls        []Sysctl
}

type Sysctl struct {
	Name string
}

type PodSpec struct {
	HostNetwork      bool
	HostPID          bool
	HostIPC          bool
	SecurityContext  *PodSecurityContext
	Containers       []Container
	InitContainers   []Container
	Volumes          []Volume
	RuntimeClassName *string
}

type PodMetadata struct {
	Name      string
	Namespace string
}

// =============================================================================
// 3. Check 타입 정의 (policy/checks.go 대응)
// =============================================================================

type CheckID string

// CheckResult는 개별 체크의 결과이다.
type CheckResult struct {
	Allowed         bool
	ForbiddenReason string
	ForbiddenDetail string
}

// CheckPodFn은 체크 함수 시그니처이다.
type CheckPodFn func(metadata *PodMetadata, spec *PodSpec) CheckResult

// VersionedCheck는 특정 버전부터 적용되는 체크이다.
type VersionedCheck struct {
	MinimumVersion   Version
	CheckPod         CheckPodFn
	OverrideCheckIDs []CheckID
}

// Check는 하나의 보안 체크를 정의한다.
type Check struct {
	ID       CheckID
	Level    Level
	Versions []VersionedCheck
}

// =============================================================================
// 4. Check Registry (policy/registry.go 대응)
// =============================================================================

// CheckRegistry는 버전별 체크 함수를 관리한다.
type CheckRegistry struct {
	baselineChecks   map[Version][]CheckPodFn
	restrictedChecks map[Version][]CheckPodFn
	maxVersion       Version
}

// versionedEntry는 버전별 체크 맵 구축에 사용되는 내부 타입이다.
type versionedEntry struct {
	checkPod         CheckPodFn
	overrideCheckIDs []CheckID
}

// NewCheckRegistry는 체크 목록으로부터 레지스트리를 구축한다.
func NewCheckRegistry(checks []Check) *CheckRegistry {
	r := &CheckRegistry{
		baselineChecks:   map[Version][]CheckPodFn{},
		restrictedChecks: map[Version][]CheckPodFn{},
	}

	// maxVersion 찾기
	for _, c := range checks {
		lastVer := c.Versions[len(c.Versions)-1].MinimumVersion
		if r.maxVersion.Older(lastVer) {
			r.maxVersion = lastVer
		}
	}

	// 버전별 체크 맵 구축
	restrictedVersioned := map[Version]map[CheckID]versionedEntry{}
	baselineVersioned := map[Version]map[CheckID]versionedEntry{}

	var baselineIDs, restrictedIDs []CheckID

	for _, c := range checks {
		target := baselineVersioned
		if c.Level == LevelRestricted {
			target = restrictedVersioned
			restrictedIDs = append(restrictedIDs, c.ID)
		} else {
			baselineIDs = append(baselineIDs, c.ID)
		}

		// 버전 인플레이션
		for i, vc := range c.Versions {
			var nextVer Version
			if i+1 < len(c.Versions) {
				nextVer = c.Versions[i+1].MinimumVersion
			} else {
				nextVer = nextMinor(r.maxVersion)
			}
			for v := vc.MinimumVersion; v.Older(nextVer); v = nextMinor(v) {
				if target[v] == nil {
					target[v] = map[CheckID]versionedEntry{}
				}
				target[v][c.ID] = versionedEntry{
					checkPod:         vc.CheckPod,
					overrideCheckIDs: vc.OverrideCheckIDs,
				}
			}
		}
	}

	// ID 정렬 (일관된 순서 유지)
	sort.Slice(baselineIDs, func(i, j int) bool { return baselineIDs[i] < baselineIDs[j] })
	sort.Slice(restrictedIDs, func(i, j int) bool { return restrictedIDs[i] < restrictedIDs[j] })
	orderedIDs := append(baselineIDs, restrictedIDs...)

	// 각 버전에 대해 최종 체크 목록 구축
	for v := (Version{1, 0}); v.Older(nextMinor(r.maxVersion)); v = nextMinor(v) {
		// 오버라이드 수집
		overrides := map[CheckID]bool{}
		for _, entry := range restrictedVersioned[v] {
			for _, override := range entry.overrideCheckIDs {
				overrides[override] = true
			}
		}

		// 오버라이드되지 않은 baseline 체크를 restricted에 추가
		for id, entry := range baselineVersioned[v] {
			if overrides[id] {
				continue
			}
			if restrictedVersioned[v] == nil {
				restrictedVersioned[v] = map[CheckID]versionedEntry{}
			}
			restrictedVersioned[v][id] = entry
		}

		// 정렬된 순서로 함수 슬라이스 생성
		r.baselineChecks[v] = mapToSlice(baselineVersioned[v], orderedIDs)
		r.restrictedChecks[v] = mapToSlice(restrictedVersioned[v], orderedIDs)
	}

	return r
}

func mapToSlice(entries map[CheckID]versionedEntry, orderedIDs []CheckID) []CheckPodFn {
	var fns []CheckPodFn
	for _, id := range orderedIDs {
		if entry, ok := entries[id]; ok {
			fns = append(fns, entry.checkPod)
		}
	}
	return fns
}

// EvaluatePod는 주어진 레벨+버전에 대해 Pod를 평가한다.
func (r *CheckRegistry) EvaluatePod(lv LevelVersion, metadata *PodMetadata, spec *PodSpec) []CheckResult {
	if lv.Level == LevelPrivileged {
		return nil // Privileged는 체크 없음
	}

	version := lv.Version
	if r.maxVersion.Older(version) {
		version = r.maxVersion
	}

	var checks []CheckPodFn
	if lv.Level == LevelBaseline {
		checks = r.baselineChecks[version]
	} else {
		checks = r.restrictedChecks[version]
	}

	var results []CheckResult
	for _, check := range checks {
		results = append(results, check(metadata, spec))
	}
	return results
}

// =============================================================================
// 5. 체크 구현 (policy/check_*.go 대응)
// =============================================================================

// visitContainers는 init 컨테이너와 일반 컨테이너를 모두 순회한다.
func visitContainers(spec *PodSpec, fn func(*Container)) {
	for i := range spec.InitContainers {
		fn(&spec.InitContainers[i])
	}
	for i := range spec.Containers {
		fn(&spec.Containers[i])
	}
}

// --- Baseline 체크들 ---

func checkPrivileged() Check {
	return Check{
		ID: "privileged", Level: LevelBaseline,
		Versions: []VersionedCheck{{
			MinimumVersion: Version{1, 0},
			CheckPod: func(m *PodMetadata, s *PodSpec) CheckResult {
				var bad []string
				visitContainers(s, func(c *Container) {
					if c.SecurityContext != nil && c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged {
						bad = append(bad, c.Name)
					}
				})
				if len(bad) > 0 {
					return CheckResult{false, "privileged",
						fmt.Sprintf("containers %s must not set securityContext.privileged=true", strings.Join(bad, ", "))}
				}
				return CheckResult{Allowed: true}
			},
		}},
	}
}

func checkHostNamespaces() Check {
	return Check{
		ID: "hostNamespaces", Level: LevelBaseline,
		Versions: []VersionedCheck{{
			MinimumVersion: Version{1, 0},
			CheckPod: func(m *PodMetadata, s *PodSpec) CheckResult {
				var violations []string
				if s.HostNetwork {
					violations = append(violations, "hostNetwork=true")
				}
				if s.HostPID {
					violations = append(violations, "hostPID=true")
				}
				if s.HostIPC {
					violations = append(violations, "hostIPC=true")
				}
				if len(violations) > 0 {
					return CheckResult{false, "host namespaces", strings.Join(violations, ", ")}
				}
				return CheckResult{Allowed: true}
			},
		}},
	}
}

func checkHostPorts() Check {
	return Check{
		ID: "hostPorts", Level: LevelBaseline,
		Versions: []VersionedCheck{{
			MinimumVersion: Version{1, 0},
			CheckPod: func(m *PodMetadata, s *PodSpec) CheckResult {
				var bad []string
				visitContainers(s, func(c *Container) {
					for _, p := range c.Ports {
						if p.HostPort != 0 {
							bad = append(bad, c.Name)
							break
						}
					}
				})
				if len(bad) > 0 {
					return CheckResult{false, "hostPort",
						fmt.Sprintf("containers %s use hostPorts", strings.Join(bad, ", "))}
				}
				return CheckResult{Allowed: true}
			},
		}},
	}
}

func checkHostPathVolumes() Check {
	return Check{
		ID: "hostPathVolumes", Level: LevelBaseline,
		Versions: []VersionedCheck{{
			MinimumVersion: Version{1, 0},
			CheckPod: func(m *PodMetadata, s *PodSpec) CheckResult {
				var bad []string
				for _, v := range s.Volumes {
					if v.Type == "hostPath" {
						bad = append(bad, v.Name)
					}
				}
				if len(bad) > 0 {
					return CheckResult{false, "hostPath volumes",
						fmt.Sprintf("volumes %s use hostPath", strings.Join(bad, ", "))}
				}
				return CheckResult{Allowed: true}
			},
		}},
	}
}

var capabilitiesAllowedBaseline = map[string]bool{
	"AUDIT_WRITE": true, "CHOWN": true, "DAC_OVERRIDE": true,
	"FOWNER": true, "FSETID": true, "KILL": true, "MKNOD": true,
	"NET_BIND_SERVICE": true, "SETFCAP": true, "SETGID": true,
	"SETPCAP": true, "SETUID": true, "SYS_CHROOT": true,
}

func checkCapabilitiesBaseline() Check {
	return Check{
		ID: "capabilities_baseline", Level: LevelBaseline,
		Versions: []VersionedCheck{{
			MinimumVersion: Version{1, 0},
			CheckPod: func(m *PodMetadata, s *PodSpec) CheckResult {
				var bad []string
				var forbidden []string
				visitContainers(s, func(c *Container) {
					if c.SecurityContext != nil && c.SecurityContext.Capabilities != nil {
						for _, cap := range c.SecurityContext.Capabilities.Add {
							if !capabilitiesAllowedBaseline[cap] {
								bad = append(bad, c.Name)
								forbidden = append(forbidden, cap)
								break
							}
						}
					}
				})
				if len(bad) > 0 {
					return CheckResult{false, "non-default capabilities",
						fmt.Sprintf("containers %s add forbidden capabilities: %s",
							strings.Join(bad, ", "), strings.Join(forbidden, ", "))}
				}
				return CheckResult{Allowed: true}
			},
		}},
	}
}

var sysctlsAllowedV1_0 = map[string]bool{
	"kernel.shm_rmid_forced":              true,
	"net.ipv4.ip_local_port_range":        true,
	"net.ipv4.tcp_syncookies":             true,
	"net.ipv4.ping_group_range":           true,
	"net.ipv4.ip_unprivileged_port_start": true,
}

var sysctlsAllowedV1_27 = mergeMaps(sysctlsAllowedV1_0, map[string]bool{
	"net.ipv4.ip_local_reserved_ports": true,
})

func mergeMaps(base, extra map[string]bool) map[string]bool {
	result := make(map[string]bool, len(base)+len(extra))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range extra {
		result[k] = v
	}
	return result
}

func makeSysctlsCheck(allowed map[string]bool) CheckPodFn {
	return func(m *PodMetadata, s *PodSpec) CheckResult {
		if s.SecurityContext == nil {
			return CheckResult{Allowed: true}
		}
		var bad []string
		for _, sc := range s.SecurityContext.Sysctls {
			if !allowed[sc.Name] {
				bad = append(bad, sc.Name)
			}
		}
		if len(bad) > 0 {
			return CheckResult{false, "forbidden sysctls", strings.Join(bad, ", ")}
		}
		return CheckResult{Allowed: true}
	}
}

func checkSysctls() Check {
	return Check{
		ID: "sysctls", Level: LevelBaseline,
		Versions: []VersionedCheck{
			{MinimumVersion: Version{1, 0}, CheckPod: makeSysctlsCheck(sysctlsAllowedV1_0)},
			{MinimumVersion: Version{1, 27}, CheckPod: makeSysctlsCheck(sysctlsAllowedV1_27)},
		},
	}
}

func checkProcMount() Check {
	return Check{
		ID: "procMount", Level: LevelBaseline,
		Versions: []VersionedCheck{{
			MinimumVersion: Version{1, 0},
			CheckPod: func(m *PodMetadata, s *PodSpec) CheckResult {
				var bad []string
				visitContainers(s, func(c *Container) {
					if c.SecurityContext != nil && c.SecurityContext.ProcMount != nil &&
						*c.SecurityContext.ProcMount != "Default" {
						bad = append(bad, c.Name)
					}
				})
				if len(bad) > 0 {
					return CheckResult{false, "procMount",
						fmt.Sprintf("containers %s set forbidden procMount", strings.Join(bad, ", "))}
				}
				return CheckResult{Allowed: true}
			},
		}},
	}
}

func checkSeccompBaseline() Check {
	return Check{
		ID: "seccompProfile_baseline", Level: LevelBaseline,
		Versions: []VersionedCheck{{
			MinimumVersion: Version{1, 19},
			CheckPod: func(m *PodMetadata, s *PodSpec) CheckResult {
				validSeccomp := func(t string) bool {
					return t == "RuntimeDefault" || t == "Localhost"
				}
				var bad []string
				if s.SecurityContext != nil && s.SecurityContext.SeccompProfile != nil {
					if !validSeccomp(*s.SecurityContext.SeccompProfile) {
						bad = append(bad, "pod")
					}
				}
				visitContainers(s, func(c *Container) {
					if c.SecurityContext != nil && c.SecurityContext.SeccompProfile != nil {
						if !validSeccomp(*c.SecurityContext.SeccompProfile) {
							bad = append(bad, c.Name)
						}
					}
				})
				if len(bad) > 0 {
					return CheckResult{false, "seccompProfile",
						fmt.Sprintf("%s set forbidden seccompProfile", strings.Join(bad, ", "))}
				}
				return CheckResult{Allowed: true}
			},
		}},
	}
}

// --- Restricted 체크들 ---

func checkRunAsNonRoot() Check {
	return Check{
		ID: "runAsNonRoot", Level: LevelRestricted,
		Versions: []VersionedCheck{{
			MinimumVersion: Version{1, 0},
			CheckPod: func(m *PodMetadata, s *PodSpec) CheckResult {
				podRunAsNonRoot := false
				var badSetters []string

				if s.SecurityContext != nil && s.SecurityContext.RunAsNonRoot != nil {
					if !*s.SecurityContext.RunAsNonRoot {
						badSetters = append(badSetters, "pod")
					} else {
						podRunAsNonRoot = true
					}
				}

				var explicitBad, implicitBad []string
				visitContainers(s, func(c *Container) {
					if c.SecurityContext != nil && c.SecurityContext.RunAsNonRoot != nil {
						if !*c.SecurityContext.RunAsNonRoot {
							explicitBad = append(explicitBad, c.Name)
						}
					} else if !podRunAsNonRoot {
						implicitBad = append(implicitBad, c.Name)
					}
				})

				if len(explicitBad) > 0 {
					badSetters = append(badSetters, fmt.Sprintf("containers %s", strings.Join(explicitBad, ", ")))
				}
				if len(badSetters) > 0 {
					return CheckResult{false, "runAsNonRoot != true",
						fmt.Sprintf("%s must not set runAsNonRoot=false", strings.Join(badSetters, " and "))}
				}
				if len(implicitBad) > 0 {
					return CheckResult{false, "runAsNonRoot != true",
						fmt.Sprintf("pod or containers %s must set runAsNonRoot=true", strings.Join(implicitBad, ", "))}
				}
				return CheckResult{Allowed: true}
			},
		}},
	}
}

func checkAllowPrivilegeEscalation() Check {
	return Check{
		ID: "allowPrivilegeEscalation", Level: LevelRestricted,
		Versions: []VersionedCheck{{
			MinimumVersion: Version{1, 8},
			CheckPod: func(m *PodMetadata, s *PodSpec) CheckResult {
				var bad []string
				visitContainers(s, func(c *Container) {
					if c.SecurityContext == nil ||
						c.SecurityContext.AllowPrivilegeEscalation == nil ||
						*c.SecurityContext.AllowPrivilegeEscalation {
						bad = append(bad, c.Name)
					}
				})
				if len(bad) > 0 {
					return CheckResult{false, "allowPrivilegeEscalation != false",
						fmt.Sprintf("containers %s must set allowPrivilegeEscalation=false", strings.Join(bad, ", "))}
				}
				return CheckResult{Allowed: true}
			},
		}},
	}
}

func checkCapabilitiesRestricted() Check {
	return Check{
		ID: "capabilities_restricted", Level: LevelRestricted,
		Versions: []VersionedCheck{{
			MinimumVersion:   Version{1, 22},
			OverrideCheckIDs: []CheckID{"capabilities_baseline"},
			CheckPod: func(m *PodMetadata, s *PodSpec) CheckResult {
				var missingDropAll, addingForbidden []string
				var forbiddenCaps []string

				visitContainers(s, func(c *Container) {
					if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil {
						missingDropAll = append(missingDropAll, c.Name)
						return
					}
					droppedAll := false
					for _, cap := range c.SecurityContext.Capabilities.Drop {
						if cap == "ALL" {
							droppedAll = true
							break
						}
					}
					if !droppedAll {
						missingDropAll = append(missingDropAll, c.Name)
					}
					for _, cap := range c.SecurityContext.Capabilities.Add {
						if cap != "NET_BIND_SERVICE" {
							addingForbidden = append(addingForbidden, c.Name)
							forbiddenCaps = append(forbiddenCaps, cap)
						}
					}
				})

				var details []string
				if len(missingDropAll) > 0 {
					details = append(details,
						fmt.Sprintf("containers %s must set capabilities.drop=[\"ALL\"]",
							strings.Join(missingDropAll, ", ")))
				}
				if len(addingForbidden) > 0 {
					details = append(details,
						fmt.Sprintf("containers %s must not add capabilities %s",
							strings.Join(addingForbidden, ", "), strings.Join(forbiddenCaps, ", ")))
				}
				if len(details) > 0 {
					return CheckResult{false, "unrestricted capabilities", strings.Join(details, "; ")}
				}
				return CheckResult{Allowed: true}
			},
		}},
	}
}

func checkRestrictedVolumes() Check {
	allowedTypes := map[string]bool{
		"configMap": true, "downwardAPI": true, "emptyDir": true,
		"projected": true, "secret": true, "csi": true,
		"persistentVolumeClaim": true, "ephemeral": true, "image": true,
	}
	return Check{
		ID: "restrictedVolumes", Level: LevelRestricted,
		Versions: []VersionedCheck{{
			MinimumVersion:   Version{1, 0},
			OverrideCheckIDs: []CheckID{"hostPathVolumes"},
			CheckPod: func(m *PodMetadata, s *PodSpec) CheckResult {
				var bad []string
				var badTypes []string
				for _, v := range s.Volumes {
					if !allowedTypes[v.Type] {
						bad = append(bad, v.Name)
						badTypes = append(badTypes, v.Type)
					}
				}
				if len(bad) > 0 {
					return CheckResult{false, "restricted volume types",
						fmt.Sprintf("volumes %s use restricted types %s",
							strings.Join(bad, ", "), strings.Join(badTypes, ", "))}
				}
				return CheckResult{Allowed: true}
			},
		}},
	}
}

func checkSeccompRestricted() Check {
	return Check{
		ID: "seccompProfile_restricted", Level: LevelRestricted,
		Versions: []VersionedCheck{{
			MinimumVersion:   Version{1, 19},
			OverrideCheckIDs: []CheckID{"seccompProfile_baseline"},
			CheckPod: func(m *PodMetadata, s *PodSpec) CheckResult {
				validSeccomp := func(t string) bool {
					return t == "RuntimeDefault" || t == "Localhost"
				}

				podSeccompSet := false
				var badSetters []string

				if s.SecurityContext != nil && s.SecurityContext.SeccompProfile != nil {
					if !validSeccomp(*s.SecurityContext.SeccompProfile) {
						badSetters = append(badSetters, "pod")
					} else {
						podSeccompSet = true
					}
				}

				var explicitBad, implicitBad []string
				visitContainers(s, func(c *Container) {
					if c.SecurityContext != nil && c.SecurityContext.SeccompProfile != nil {
						if !validSeccomp(*c.SecurityContext.SeccompProfile) {
							explicitBad = append(explicitBad, c.Name)
						}
					} else if !podSeccompSet {
						implicitBad = append(implicitBad, c.Name)
					}
				})

				if len(explicitBad) > 0 {
					badSetters = append(badSetters, fmt.Sprintf("containers %s", strings.Join(explicitBad, ", ")))
				}
				if len(badSetters) > 0 {
					return CheckResult{false, "seccompProfile",
						fmt.Sprintf("%s set forbidden seccompProfile", strings.Join(badSetters, ", "))}
				}
				if len(implicitBad) > 0 {
					return CheckResult{false, "seccompProfile",
						fmt.Sprintf("pod or containers %s must set seccompProfile to RuntimeDefault or Localhost",
							strings.Join(implicitBad, ", "))}
				}
				return CheckResult{Allowed: true}
			},
		}},
	}
}

func checkRunAsUser() Check {
	return Check{
		ID: "runAsUser", Level: LevelRestricted,
		Versions: []VersionedCheck{{
			MinimumVersion: Version{1, 23},
			CheckPod: func(m *PodMetadata, s *PodSpec) CheckResult {
				var bad []string
				if s.SecurityContext != nil && s.SecurityContext.RunAsUser != nil && *s.SecurityContext.RunAsUser == 0 {
					bad = append(bad, "pod")
				}
				visitContainers(s, func(c *Container) {
					if c.SecurityContext != nil && c.SecurityContext.RunAsUser != nil && *c.SecurityContext.RunAsUser == 0 {
						bad = append(bad, c.Name)
					}
				})
				if len(bad) > 0 {
					return CheckResult{false, "runAsUser=0",
						fmt.Sprintf("%s must not set runAsUser=0", strings.Join(bad, " and "))}
				}
				return CheckResult{Allowed: true}
			},
		}},
	}
}

// DefaultChecks는 모든 체크를 반환한다.
func DefaultChecks() []Check {
	return []Check{
		checkPrivileged(),
		checkHostNamespaces(),
		checkHostPorts(),
		checkHostPathVolumes(),
		checkCapabilitiesBaseline(),
		checkSysctls(),
		checkProcMount(),
		checkSeccompBaseline(),
		checkRunAsNonRoot(),
		checkAllowPrivilegeEscalation(),
		checkCapabilitiesRestricted(),
		checkRestrictedVolumes(),
		checkSeccompRestricted(),
		checkRunAsUser(),
	}
}

// =============================================================================
// 6. Admission 로직 (admission/admission.go 대응)
// =============================================================================

// AggregateCheckResult는 모든 체크 결과를 집약한다.
type AggregateCheckResult struct {
	Allowed   bool
	Forbidden []CheckResult
}

func (r AggregateCheckResult) ForbiddenDetail() string {
	var details []string
	for _, f := range r.Forbidden {
		details = append(details, f.ForbiddenDetail)
	}
	return strings.Join(details, "; ")
}

func AggregateResults(results []CheckResult) AggregateCheckResult {
	agg := AggregateCheckResult{Allowed: true}
	for _, r := range results {
		if !r.Allowed {
			agg.Allowed = false
			agg.Forbidden = append(agg.Forbidden, r)
		}
	}
	return agg
}

// ExemptionConfig는 면제 설정이다.
type ExemptionConfig struct {
	Namespaces     []string
	Usernames      []string
	RuntimeClasses []string
}

// Admission은 PSA의 핵심 로직이다.
type Admission struct {
	Registry   *CheckRegistry
	Exemptions ExemptionConfig
}

// EvaluationResult는 평가 결과이다.
type EvaluationResult struct {
	Allowed    bool
	Reason     string   // Enforce 거부 사유
	Audit      string   // Audit annotation
	Warnings   []string // Warn 경고
	Exempted   bool
	ExemptType string
}

func containsString(s string, list []string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// ParsePolicyFromLabels는 Namespace 라벨에서 Policy를 파싱한다.
func ParsePolicyFromLabels(labels map[string]string) Policy {
	parseLevel := func(key string) Level {
		switch labels[key] {
		case "baseline":
			return LevelBaseline
		case "restricted":
			return LevelRestricted
		default:
			return LevelPrivileged
		}
	}

	parseVersion := func(key string) Version {
		v := labels[key]
		if v == "" || v == "latest" {
			return Version{1, 35} // 시뮬레이션에서의 최신 버전
		}
		// 간단한 파싱: v1.XX
		var major, minor int
		fmt.Sscanf(v, "v%d.%d", &major, &minor)
		if major == 0 {
			return Version{1, 35}
		}
		return Version{major, minor}
	}

	return Policy{
		Enforce: LevelVersion{
			Level:   parseLevel("pod-security.kubernetes.io/enforce"),
			Version: parseVersion("pod-security.kubernetes.io/enforce-version"),
		},
		Audit: LevelVersion{
			Level:   parseLevel("pod-security.kubernetes.io/audit"),
			Version: parseVersion("pod-security.kubernetes.io/audit-version"),
		},
		Warn: LevelVersion{
			Level:   parseLevel("pod-security.kubernetes.io/warn"),
			Version: parseVersion("pod-security.kubernetes.io/warn-version"),
		},
	}
}

// Evaluate는 Pod를 정책에 대해 평가한다.
func (a *Admission) Evaluate(
	policy Policy,
	metadata *PodMetadata,
	spec *PodSpec,
	username string,
	enforce bool,
) EvaluationResult {

	// 1. 네임스페이스 면제
	if containsString(metadata.Namespace, a.Exemptions.Namespaces) {
		return EvaluationResult{Allowed: true, Exempted: true, ExemptType: "namespace"}
	}

	// 2. 사용자 면제
	if containsString(username, a.Exemptions.Usernames) {
		return EvaluationResult{Allowed: true, Exempted: true, ExemptType: "user"}
	}

	// 3. FullyPrivileged 단축 경로
	if policy.FullyPrivileged() {
		return EvaluationResult{Allowed: true}
	}

	// 4. RuntimeClass 면제
	if spec.RuntimeClassName != nil && containsString(*spec.RuntimeClassName, a.Exemptions.RuntimeClasses) {
		return EvaluationResult{Allowed: true, Exempted: true, ExemptType: "runtimeClass"}
	}

	result := EvaluationResult{Allowed: true}
	cachedResults := map[string]AggregateCheckResult{}

	// 5. Enforce 평가
	if enforce {
		key := policy.Enforce.String()
		enforceResult := AggregateResults(a.Registry.EvaluatePod(policy.Enforce, metadata, spec))
		cachedResults[key] = enforceResult
		if !enforceResult.Allowed {
			result.Allowed = false
			result.Reason = fmt.Sprintf("violates PodSecurity %q: %s",
				policy.Enforce.String(), enforceResult.ForbiddenDetail())
		}
	}

	// 6. Audit 평가 (캐시 재사용)
	auditKey := policy.Audit.String()
	auditResult, ok := cachedResults[auditKey]
	if !ok {
		auditResult = AggregateResults(a.Registry.EvaluatePod(policy.Audit, metadata, spec))
		cachedResults[auditKey] = auditResult
	}
	if !auditResult.Allowed {
		result.Audit = fmt.Sprintf("would violate PodSecurity %q: %s",
			policy.Audit.String(), auditResult.ForbiddenDetail())
	}

	// 7. Warn 평가 (거부된 요청에는 경고 추가 안 함)
	if result.Allowed {
		warnKey := policy.Warn.String()
		warnResult, ok := cachedResults[warnKey]
		if !ok {
			warnResult = AggregateResults(a.Registry.EvaluatePod(policy.Warn, metadata, spec))
		}
		if !warnResult.Allowed {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("would violate PodSecurity %q: %s",
					policy.Warn.String(), warnResult.ForbiddenDetail()))
		}
	}

	return result
}

// =============================================================================
// 7. 메인 - 데모 시나리오
// =============================================================================

func boolPtr(b bool) *bool       { return &b }
func int64Ptr(i int64) *int64    { return &i }
func strPtr(s string) *string    { return &s }

func printSeparator(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf("  %s\n", title)
	fmt.Println(strings.Repeat("=", 70))
}

func printResult(name string, result EvaluationResult) {
	status := "ALLOWED"
	if !result.Allowed {
		status = "DENIED"
	}
	fmt.Printf("\n  [%s] %s\n", status, name)
	if result.Exempted {
		fmt.Printf("    면제 적용: %s\n", result.ExemptType)
	}
	if result.Reason != "" {
		fmt.Printf("    거부 사유: %s\n", result.Reason)
	}
	if result.Audit != "" {
		fmt.Printf("    감사 기록: %s\n", result.Audit)
	}
	for _, w := range result.Warnings {
		fmt.Printf("    경고: %s\n", w)
	}
}

func main() {
	fmt.Println("Pod Security Admission (PSA) 핵심 알고리즘 시뮬레이션")
	fmt.Println("Kubernetes 소스 재현: staging/src/k8s.io/pod-security-admission/")

	// 레지스트리 초기화
	registry := NewCheckRegistry(DefaultChecks())

	admission := &Admission{
		Registry: registry,
		Exemptions: ExemptionConfig{
			Namespaces:     []string{"kube-system", "kube-public"},
			Usernames:      []string{"system:serviceaccount:kube-system:replicaset-controller"},
			RuntimeClasses: []string{"gvisor"},
		},
	}

	// =========================================================================
	// 시나리오 1: Security Level별 평가
	// =========================================================================
	printSeparator("시나리오 1: Security Level별 평가")

	// 특권 컨테이너 Pod
	privilegedPod := &PodSpec{
		Containers: []Container{{
			Name: "nginx",
			SecurityContext: &SecurityContext{
				Privileged: boolPtr(true),
			},
		}},
	}
	privilegedMeta := &PodMetadata{Name: "privileged-pod", Namespace: "default"}

	fmt.Println("\n--- 특권 컨테이너 Pod ---")

	// Privileged 레벨: 통과
	result := admission.Evaluate(
		Policy{
			Enforce: LevelVersion{LevelPrivileged, Version{1, 28}},
			Audit:   LevelVersion{LevelPrivileged, Version{1, 28}},
			Warn:    LevelVersion{LevelPrivileged, Version{1, 28}},
		},
		privilegedMeta, privilegedPod, "user1", true)
	printResult("Privileged 레벨에서 특권 Pod", result)

	// Baseline 레벨: 거부
	result = admission.Evaluate(
		Policy{
			Enforce: LevelVersion{LevelBaseline, Version{1, 28}},
			Audit:   LevelVersion{LevelBaseline, Version{1, 28}},
			Warn:    LevelVersion{LevelBaseline, Version{1, 28}},
		},
		privilegedMeta, privilegedPod, "user1", true)
	printResult("Baseline 레벨에서 특권 Pod", result)

	// =========================================================================
	// 시나리오 2: Baseline vs Restricted 차이
	// =========================================================================
	printSeparator("시나리오 2: Baseline vs Restricted 차이")

	// Baseline은 통과하지만 Restricted는 거부되는 Pod
	baselineOkPod := &PodSpec{
		Containers: []Container{{
			Name: "app",
			SecurityContext: &SecurityContext{
				Privileged:               boolPtr(false),
				AllowPrivilegeEscalation: boolPtr(true), // Restricted에서 거부됨
			},
		}},
	}
	baselineOkMeta := &PodMetadata{Name: "baseline-ok-pod", Namespace: "default"}

	// Baseline: 통과
	result = admission.Evaluate(
		Policy{
			Enforce: LevelVersion{LevelBaseline, Version{1, 28}},
			Audit:   LevelVersion{LevelBaseline, Version{1, 28}},
			Warn:    LevelVersion{LevelBaseline, Version{1, 28}},
		},
		baselineOkMeta, baselineOkPod, "user1", true)
	printResult("Baseline: allowPrivilegeEscalation=true", result)

	// Restricted: 거부
	result = admission.Evaluate(
		Policy{
			Enforce: LevelVersion{LevelRestricted, Version{1, 28}},
			Audit:   LevelVersion{LevelRestricted, Version{1, 28}},
			Warn:    LevelVersion{LevelRestricted, Version{1, 28}},
		},
		baselineOkMeta, baselineOkPod, "user1", true)
	printResult("Restricted: allowPrivilegeEscalation=true", result)

	// =========================================================================
	// 시나리오 3: Enforce/Audit/Warn 모드 분리
	// =========================================================================
	printSeparator("시나리오 3: Enforce/Audit/Warn 모드 분리")

	// hostNetwork Pod
	hostNetPod := &PodSpec{
		HostNetwork: true,
		Containers: []Container{{
			Name: "monitor",
			SecurityContext: &SecurityContext{
				Privileged: boolPtr(false),
			},
		}},
	}
	hostNetMeta := &PodMetadata{Name: "hostnet-pod", Namespace: "monitoring"}

	// Enforce=privileged, Audit=baseline, Warn=baseline
	// -> 허용되지만 Audit/Warn 기록
	result = admission.Evaluate(
		Policy{
			Enforce: LevelVersion{LevelPrivileged, Version{1, 28}},
			Audit:   LevelVersion{LevelBaseline, Version{1, 28}},
			Warn:    LevelVersion{LevelBaseline, Version{1, 28}},
		},
		hostNetMeta, hostNetPod, "user1", true)
	printResult("Enforce=privileged, Audit/Warn=baseline", result)

	// Enforce=baseline -> 거부
	result = admission.Evaluate(
		Policy{
			Enforce: LevelVersion{LevelBaseline, Version{1, 28}},
			Audit:   LevelVersion{LevelBaseline, Version{1, 28}},
			Warn:    LevelVersion{LevelBaseline, Version{1, 28}},
		},
		hostNetMeta, hostNetPod, "user1", true)
	printResult("Enforce=baseline (거부됨)", result)

	// =========================================================================
	// 시나리오 4: 버전별 체크 차이 (sysctls)
	// =========================================================================
	printSeparator("시나리오 4: 버전별 체크 차이 (sysctls)")

	sysctlPod := &PodSpec{
		SecurityContext: &PodSecurityContext{
			Sysctls: []Sysctl{
				{Name: "net.ipv4.ip_local_reserved_ports"}, // v1.27+에서 허용
			},
		},
		Containers: []Container{{Name: "app"}},
	}
	sysctlMeta := &PodMetadata{Name: "sysctl-pod", Namespace: "default"}

	// v1.25 기준: 거부 (이 sysctl은 v1.27부터 허용)
	result = admission.Evaluate(
		Policy{
			Enforce: LevelVersion{LevelBaseline, Version{1, 25}},
			Audit:   LevelVersion{LevelBaseline, Version{1, 25}},
			Warn:    LevelVersion{LevelBaseline, Version{1, 25}},
		},
		sysctlMeta, sysctlPod, "user1", true)
	printResult("v1.25 기준: ip_local_reserved_ports", result)

	// v1.27 기준: 허용
	result = admission.Evaluate(
		Policy{
			Enforce: LevelVersion{LevelBaseline, Version{1, 27}},
			Audit:   LevelVersion{LevelBaseline, Version{1, 27}},
			Warn:    LevelVersion{LevelBaseline, Version{1, 27}},
		},
		sysctlMeta, sysctlPod, "user1", true)
	printResult("v1.27 기준: ip_local_reserved_ports", result)

	// =========================================================================
	// 시나리오 5: Exemption 시스템
	// =========================================================================
	printSeparator("시나리오 5: Exemption 시스템")

	violatingPod := &PodSpec{
		HostNetwork: true,
		HostPID:     true,
		Containers: []Container{{
			Name: "system",
			SecurityContext: &SecurityContext{
				Privileged: boolPtr(true),
			},
		}},
	}

	// 5a. 네임스페이스 면제
	ksMeta := &PodMetadata{Name: "kube-proxy", Namespace: "kube-system"}
	result = admission.Evaluate(
		Policy{
			Enforce: LevelVersion{LevelRestricted, Version{1, 28}},
			Audit:   LevelVersion{LevelRestricted, Version{1, 28}},
			Warn:    LevelVersion{LevelRestricted, Version{1, 28}},
		},
		ksMeta, violatingPod, "admin", true)
	printResult("kube-system 네임스페이스 면제", result)

	// 5b. 사용자 면제
	defaultMeta := &PodMetadata{Name: "rs-pod", Namespace: "default"}
	result = admission.Evaluate(
		Policy{
			Enforce: LevelVersion{LevelRestricted, Version{1, 28}},
			Audit:   LevelVersion{LevelRestricted, Version{1, 28}},
			Warn:    LevelVersion{LevelRestricted, Version{1, 28}},
		},
		defaultMeta, violatingPod,
		"system:serviceaccount:kube-system:replicaset-controller", true)
	printResult("시스템 서비스 계정 면제", result)

	// 5c. RuntimeClass 면제
	gvisorPod := &PodSpec{
		HostNetwork:      true,
		RuntimeClassName: strPtr("gvisor"),
		Containers: []Container{{
			Name: "sandboxed",
			SecurityContext: &SecurityContext{
				Privileged: boolPtr(true),
			},
		}},
	}
	gvisorMeta := &PodMetadata{Name: "gvisor-pod", Namespace: "default"}
	result = admission.Evaluate(
		Policy{
			Enforce: LevelVersion{LevelRestricted, Version{1, 28}},
			Audit:   LevelVersion{LevelRestricted, Version{1, 28}},
			Warn:    LevelVersion{LevelRestricted, Version{1, 28}},
		},
		gvisorMeta, gvisorPod, "user1", true)
	printResult("gvisor RuntimeClass 면제", result)

	// =========================================================================
	// 시나리오 6: 오버라이드 시스템 데모
	// =========================================================================
	printSeparator("시나리오 6: 오버라이드 시스템 데모")

	// capabilities: Baseline에서는 기본 capability 허용, Restricted에서는 ALL drop 필수
	capsPod := &PodSpec{
		Containers: []Container{{
			Name: "web",
			SecurityContext: &SecurityContext{
				RunAsNonRoot:             boolPtr(true),
				AllowPrivilegeEscalation: boolPtr(false),
				SeccompProfile:           strPtr("RuntimeDefault"),
				Capabilities: &Capabilities{
					Add: []string{"CHOWN", "NET_BIND_SERVICE"}, // Baseline: OK
					// Drop ALL 미설정 -> Restricted: 거부
				},
			},
		}},
	}
	capsMeta := &PodMetadata{Name: "caps-pod", Namespace: "default"}

	// Baseline: capabilities_baseline이 적용됨 -> CHOWN은 기본 허용 목록에 있으므로 통과
	result = admission.Evaluate(
		Policy{
			Enforce: LevelVersion{LevelBaseline, Version{1, 28}},
			Audit:   LevelVersion{LevelBaseline, Version{1, 28}},
			Warn:    LevelVersion{LevelBaseline, Version{1, 28}},
		},
		capsMeta, capsPod, "user1", true)
	printResult("Baseline: CHOWN+NET_BIND_SERVICE add (capabilities_baseline)", result)

	// Restricted: capabilities_restricted가 capabilities_baseline을 오버라이드
	// -> ALL drop 필수, CHOWN add 불가
	result = admission.Evaluate(
		Policy{
			Enforce: LevelVersion{LevelRestricted, Version{1, 28}},
			Audit:   LevelVersion{LevelRestricted, Version{1, 28}},
			Warn:    LevelVersion{LevelRestricted, Version{1, 28}},
		},
		capsMeta, capsPod, "user1", true)
	printResult("Restricted: CHOWN+NET_BIND_SERVICE add (capabilities_restricted 오버라이드)", result)

	// =========================================================================
	// 시나리오 7: Restricted 레벨 완전 통과 Pod
	// =========================================================================
	printSeparator("시나리오 7: Restricted 레벨 완전 통과 Pod")

	securePod := &PodSpec{
		SecurityContext: &PodSecurityContext{
			RunAsNonRoot:   boolPtr(true),
			RunAsUser:      int64Ptr(1000),
			SeccompProfile: strPtr("RuntimeDefault"),
		},
		Containers: []Container{{
			Name: "secure-app",
			SecurityContext: &SecurityContext{
				AllowPrivilegeEscalation: boolPtr(false),
				Capabilities: &Capabilities{
					Drop: []string{"ALL"},
					Add:  []string{"NET_BIND_SERVICE"},
				},
			},
		}},
		Volumes: []Volume{
			{Name: "config", Type: "configMap"},
			{Name: "data", Type: "emptyDir"},
		},
	}
	secureMeta := &PodMetadata{Name: "secure-pod", Namespace: "production"}

	result = admission.Evaluate(
		Policy{
			Enforce: LevelVersion{LevelRestricted, Version{1, 28}},
			Audit:   LevelVersion{LevelRestricted, Version{1, 28}},
			Warn:    LevelVersion{LevelRestricted, Version{1, 28}},
		},
		secureMeta, securePod, "developer", true)
	printResult("Restricted 완전 통과 Pod", result)

	// =========================================================================
	// 시나리오 8: Namespace 라벨 파싱
	// =========================================================================
	printSeparator("시나리오 8: Namespace 라벨 파싱")

	labels := map[string]string{
		"pod-security.kubernetes.io/enforce":         "baseline",
		"pod-security.kubernetes.io/enforce-version": "v1.28",
		"pod-security.kubernetes.io/audit":           "restricted",
		"pod-security.kubernetes.io/audit-version":   "latest",
		"pod-security.kubernetes.io/warn":            "restricted",
		"pod-security.kubernetes.io/warn-version":    "latest",
	}
	policy := ParsePolicyFromLabels(labels)
	fmt.Printf("\n  라벨 파싱 결과:\n")
	fmt.Printf("    Enforce: %s\n", policy.Enforce)
	fmt.Printf("    Audit:   %s\n", policy.Audit)
	fmt.Printf("    Warn:    %s\n", policy.Warn)

	// 이 정책으로 hostNetwork Pod 평가
	result = admission.Evaluate(policy, hostNetMeta, hostNetPod, "user1", true)
	printResult("파싱된 정책으로 hostNetwork Pod 평가", result)

	// =========================================================================
	// 시나리오 9: 결과 캐싱 데모
	// =========================================================================
	printSeparator("시나리오 9: 결과 캐싱 데모")

	fmt.Println("\n  Enforce=baseline:v1.28, Audit=baseline:v1.28 -> 캐시 재사용")
	fmt.Println("  Enforce와 Audit이 동일한 LevelVersion이면 Evaluator를 한 번만 호출한다.")
	fmt.Println("  (실제 소스에서는 cachedResults map으로 구현)")

	samePolicy := Policy{
		Enforce: LevelVersion{LevelBaseline, Version{1, 28}},
		Audit:   LevelVersion{LevelBaseline, Version{1, 28}},
		Warn:    LevelVersion{LevelRestricted, Version{1, 28}},
	}
	result = admission.Evaluate(samePolicy, baselineOkMeta, baselineOkPod, "user1", true)
	printResult("Enforce=Audit 캐시 재사용 (Warn만 별도 평가)", result)

	// =========================================================================
	// 요약
	// =========================================================================
	printSeparator("요약")

	fmt.Println(`
  PSA 핵심 설계 원칙:

  1. Level 계층: Privileged(0) < Baseline(12) < Restricted(+7) 체크
  2. 버전 인플레이션: 체크 정의 시점부터 다음 버전 전까지 동일 함수 적용
  3. 오버라이드: Restricted가 Baseline 체크를 더 엄격한 버전으로 대체
  4. 3-Mode 평가: Enforce(차단) / Audit(기록) / Warn(경고) 독립 평가
  5. 결과 캐싱: 동일 LevelVersion이면 평가 재사용
  6. 단축 경로: 면제, FullyPrivileged 등에서 조기 반환
  7. Namespace 라벨: pod-security.kubernetes.io/* 라벨로 정책 설정

  소스 경로:
    api/constants.go        - Level, 라벨 상수
    api/helpers.go          - Version, Policy 타입
    policy/checks.go        - Check, VersionedCheck 타입
    policy/registry.go      - checkRegistry, 버전 인플레이션
    policy/check_*.go       - 19개 체크 구현
    admission/admission.go  - Admission, Validate, EvaluatePod`)
}
