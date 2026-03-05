package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// =============================================================================
// Local/OCI 이중 스토리지 시뮬레이션
//
// tart의 VMStorageLocal.swift, VMStorageOCI.swift, VMStorageHelper.swift 구현을
// Go로 재현한다.
//
// 핵심 개념:
//   - VMStorageLocal: ~/.tart/vms/{name}/ 디렉토리 기반 VM 관리
//   - VMStorageOCI: ~/.tart/cache/OCIs/{host}/{namespace}/{ref} 심볼릭 링크 캐시
//   - 태그 → 다이제스트 심볼릭 링크 (tag:latest → sha256:abc...)
//   - GC: 참조 카운트 기반 unreferenced 다이제스트 삭제
//   - VMStorageHelper: 이름 형식에 따라 Local/OCI 라우팅
//
// 참조: tart/Sources/tart/VMStorageLocal.swift
//       tart/Sources/tart/VMStorageOCI.swift
//       tart/Sources/tart/VMStorageHelper.swift
// =============================================================================

// --- VMDirectory: VM 데이터 디렉토리 (tart VMDirectory.swift 참조) ---

// VMDirectory는 개별 VM의 파일(디스크, NVRAM, 설정 등)을 담는 디렉토리이다.
// tart의 VMDirectory 클래스와 동일: baseURL 기반으로 VM 파일 관리.
type VMDirectory struct {
	BaseURL string // 디렉토리 경로
}

// Initialized는 VM 디렉토리가 초기화되었는지(config 파일 존재) 확인한다.
// tart의 VMDirectory.initialized 프로퍼티 대응.
func (vd *VMDirectory) Initialized() bool {
	_, err := os.Stat(filepath.Join(vd.BaseURL, "config.json"))
	return err == nil
}

// Initialize는 VM 디렉토리를 새로 초기화한다.
// tart의 VMDirectory.initialize(overwrite:) 대응: 디렉토리 생성 + config 파일 생성.
func (vd *VMDirectory) Initialize(overwrite bool) error {
	if !overwrite {
		if vd.Initialized() {
			return fmt.Errorf("VM 디렉토리가 이미 초기화됨: %s", vd.BaseURL)
		}
	}
	// 기존 디렉토리 삭제 후 재생성
	os.RemoveAll(vd.BaseURL)
	if err := os.MkdirAll(vd.BaseURL, 0755); err != nil {
		return err
	}
	// config.json 생성 (초기화 마커)
	configPath := filepath.Join(vd.BaseURL, "config.json")
	return os.WriteFile(configPath, []byte(`{"cpu":4,"memory":8192}`), 0644)
}

// Name은 디렉토리의 마지막 컴포넌트(VM 이름)를 반환한다.
// tart의 VMDirectory.name 프로퍼티.
func (vd *VMDirectory) Name() string {
	return filepath.Base(vd.BaseURL)
}

// Delete는 VM 디렉토리를 삭제한다.
// tart의 VMDirectory.delete() 대응.
func (vd *VMDirectory) Delete() error {
	return os.RemoveAll(vd.BaseURL)
}

// UpdateAccessDate는 VM 디렉토리의 접근 시간을 갱신한다.
// tart의 vmDir.baseURL.updateAccessDate() 대응.
func (vd *VMDirectory) UpdateAccessDate() error {
	now := time.Now()
	return os.Chtimes(vd.BaseURL, now, now)
}

// IsExplicitlyPulled는 다이제스트로 직접 Pull된 이미지인지 확인한다.
// tart의 VMDirectory.isExplicitlyPulled() 대응: .explicitly-pulled 마커 파일.
func (vd *VMDirectory) IsExplicitlyPulled() bool {
	_, err := os.Stat(filepath.Join(vd.BaseURL, ".explicitly-pulled"))
	return err == nil
}

// MarkExplicitlyPulled는 직접 Pull 마커를 설정한다.
// tart의 VMDirectory.markExplicitlyPulled() 대응.
func (vd *VMDirectory) MarkExplicitlyPulled() {
	os.WriteFile(filepath.Join(vd.BaseURL, ".explicitly-pulled"), []byte{}, 0644)
}

// --- VMStorageLocal: 로컬 VM 스토리지 (tart VMStorageLocal.swift 참조) ---

// VMStorageLocal은 파일 시스템 기반 VM 관리를 수행한다.
// tart의 VMStorageLocal 클래스: ~/.tart/vms/{name}/ 디렉토리 구조.
type VMStorageLocal struct {
	BaseURL string // ~/.tart/vms/
}

// NewVMStorageLocal은 로컬 스토리지를 초기화한다.
// tart: VMStorageLocal.init() → Config().tartHomeDir + "vms"
func NewVMStorageLocal(baseDir string) *VMStorageLocal {
	dir := filepath.Join(baseDir, "vms")
	os.MkdirAll(dir, 0755)
	return &VMStorageLocal{BaseURL: dir}
}

// vmURL은 VM 이름에 해당하는 디렉토리 경로를 반환한다.
// tart의 vmURL(_ name: String) → baseURL + name
func (s *VMStorageLocal) vmURL(name string) string {
	return filepath.Join(s.BaseURL, name)
}

// Exists는 VM 존재 여부를 확인한다.
// tart의 exists(_ name: String) → VMDirectory(baseURL: vmURL(name)).initialized
func (s *VMStorageLocal) Exists(name string) bool {
	return (&VMDirectory{BaseURL: s.vmURL(name)}).Initialized()
}

// Open은 기존 VM 디렉토리를 연다.
// tart의 open(_ name: String) → vmDir.validate + updateAccessDate.
func (s *VMStorageLocal) Open(name string) (*VMDirectory, error) {
	vmDir := &VMDirectory{BaseURL: s.vmURL(name)}
	if !vmDir.Initialized() {
		return nil, fmt.Errorf("VM '%s'이(가) 존재하지 않습니다", name)
	}
	vmDir.UpdateAccessDate()
	return vmDir, nil
}

// Create는 새 VM 디렉토리를 생성한다.
// tart의 create(_ name: String, overwrite: Bool) 대응.
func (s *VMStorageLocal) Create(name string, overwrite bool) (*VMDirectory, error) {
	vmDir := &VMDirectory{BaseURL: s.vmURL(name)}
	if err := vmDir.Initialize(overwrite); err != nil {
		return nil, err
	}
	return vmDir, nil
}

// Move는 VM 디렉토리를 이 스토리지로 이동시킨다.
// tart의 move(_ name: String, from: VMDirectory) 대응.
func (s *VMStorageLocal) Move(name string, from *VMDirectory) error {
	os.MkdirAll(s.BaseURL, 0755)
	target := s.vmURL(name)
	os.RemoveAll(target)
	return os.Rename(from.BaseURL, target)
}

// Rename은 VM 이름을 변경한다.
// tart의 rename(_ name: String, _ newName: String) 대응.
func (s *VMStorageLocal) Rename(name, newName string) error {
	return os.Rename(s.vmURL(name), s.vmURL(newName))
}

// Delete는 VM을 삭제한다.
// tart의 delete(_ name: String) 대응.
func (s *VMStorageLocal) Delete(name string) error {
	return (&VMDirectory{BaseURL: s.vmURL(name)}).Delete()
}

// List는 모든 로컬 VM을 나열한다.
// tart의 list() → contentsOfDirectory → initialized 필터.
func (s *VMStorageLocal) List() ([]struct {
	Name string
	Dir  *VMDirectory
}, error) {
	entries, err := os.ReadDir(s.BaseURL)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var result []struct {
		Name string
		Dir  *VMDirectory
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		vmDir := &VMDirectory{BaseURL: filepath.Join(s.BaseURL, entry.Name())}
		if !vmDir.Initialized() {
			continue
		}
		result = append(result, struct {
			Name string
			Dir  *VMDirectory
		}{entry.Name(), vmDir})
	}
	return result, nil
}

// --- RemoteName: OCI 원격 이미지 이름 (tart OCI/Reference/ 참조) ---

// RemoteName은 "ghcr.io/namespace/image:tag" 형태의 원격 이미지 이름이다.
// tart의 RemoteName 구조체: host, namespace, reference(tag 또는 digest).
type RemoteName struct {
	Host      string // e.g., "ghcr.io"
	Namespace string // e.g., "cirruslabs/macos"
	Reference string // e.g., "latest" (태그) 또는 "sha256:abc..." (다이제스트)
}

// String은 "host/namespace:reference" 문자열을 반환한다.
func (rn RemoteName) String() string {
	sep := ":"
	if strings.HasPrefix(rn.Reference, "sha256:") {
		sep = "@"
	}
	return rn.Host + "/" + rn.Namespace + sep + rn.Reference
}

// IsDigest는 참조가 다이제스트인지 확인한다.
func (rn RemoteName) IsDigest() bool {
	return strings.HasPrefix(rn.Reference, "sha256:")
}

// ParseRemoteName은 문자열에서 RemoteName을 파싱한다.
// tart의 RemoteName.init(_ name: String) 대응.
func ParseRemoteName(name string) (*RemoteName, error) {
	// "host/namespace:tag" 또는 "host/namespace@sha256:..." 형식
	atIdx := strings.Index(name, "@sha256:")
	if atIdx != -1 {
		hostAndNs := name[:atIdx]
		digest := name[atIdx+1:]
		host, ns := splitHostNamespace(hostAndNs)
		return &RemoteName{Host: host, Namespace: ns, Reference: digest}, nil
	}

	colonIdx := strings.LastIndex(name, ":")
	if colonIdx == -1 {
		return nil, fmt.Errorf("잘못된 원격 이름 형식: %s", name)
	}

	hostAndNs := name[:colonIdx]
	tag := name[colonIdx+1:]
	host, ns := splitHostNamespace(hostAndNs)

	return &RemoteName{Host: host, Namespace: ns, Reference: tag}, nil
}

// splitHostNamespace는 "host/namespace/..." 형태를 host와 namespace로 분리한다.
func splitHostNamespace(s string) (string, string) {
	idx := strings.Index(s, "/")
	if idx == -1 {
		return s, ""
	}
	return s[:idx], s[idx+1:]
}

// --- VMStorageOCI: OCI 캐시 스토리지 (tart VMStorageOCI.swift 참조) ---

// VMStorageOCI는 심볼릭 링크 기반 OCI 이미지 캐시를 관리한다.
// tart의 VMStorageOCI 클래스: ~/.tart/cache/OCIs/{host}/{namespace}/{ref}
//
// 태그 이미지: symlink (태그 → 다이제스트 디렉토리)
// 다이제스트 이미지: 실제 디렉토리
//
// GC 알고리즘:
//   1) 깨진 심볼릭 링크 삭제
//   2) 모든 심볼릭 링크의 대상 참조 카운트 집계
//   3) 참조 카운트 0이고 명시적 Pull이 아닌 다이제스트 삭제
type VMStorageOCI struct {
	BaseURL string // ~/.tart/cache/OCIs/
}

// NewVMStorageOCI는 OCI 캐시 스토리지를 초기화한다.
// tart: VMStorageOCI.init() → Config().tartCacheDir + "OCIs"
func NewVMStorageOCI(baseDir string) *VMStorageOCI {
	dir := filepath.Join(baseDir, "cache", "OCIs")
	os.MkdirAll(dir, 0755)
	return &VMStorageOCI{BaseURL: dir}
}

// vmURL은 RemoteName에 해당하는 캐시 경로를 반환한다.
// tart: vmURL(_ name: RemoteName) → baseURL.appendingRemoteName(name)
// 예: ~/.tart/cache/OCIs/ghcr.io/cirruslabs/macos/latest
func (s *VMStorageOCI) vmURL(name *RemoteName) string {
	return filepath.Join(s.BaseURL, name.Host, name.Namespace, name.Reference)
}

// Exists는 OCI 이미지가 캐시에 존재하는지 확인한다.
func (s *VMStorageOCI) Exists(name *RemoteName) bool {
	return (&VMDirectory{BaseURL: s.vmURL(name)}).Initialized()
}

// Open은 캐시된 OCI 이미지를 연다.
func (s *VMStorageOCI) Open(name *RemoteName) (*VMDirectory, error) {
	vmDir := &VMDirectory{BaseURL: s.vmURL(name)}
	if !vmDir.Initialized() {
		return nil, fmt.Errorf("OCI 이미지 '%s'이(가) 캐시에 없습니다", name)
	}
	vmDir.UpdateAccessDate()
	return vmDir, nil
}

// Create는 캐시에 새 OCI 이미지 디렉토리를 생성한다.
// tart의 create(_ name: RemoteName, overwrite: Bool) 대응.
func (s *VMStorageOCI) Create(name *RemoteName, overwrite bool) (*VMDirectory, error) {
	path := s.vmURL(name)
	os.MkdirAll(filepath.Dir(path), 0755)
	vmDir := &VMDirectory{BaseURL: path}
	if err := vmDir.Initialize(overwrite); err != nil {
		return nil, err
	}
	return vmDir, nil
}

// Move는 VM 디렉토리를 OCI 캐시로 이동한다.
// tart의 move(_ name: RemoteName, from: VMDirectory) 대응.
func (s *VMStorageOCI) Move(name *RemoteName, from *VMDirectory) error {
	target := s.vmURL(name)
	os.MkdirAll(filepath.Dir(target), 0755)
	os.RemoveAll(target)
	return os.Rename(from.BaseURL, target)
}

// Delete는 캐시에서 OCI 이미지를 삭제하고 GC를 실행한다.
// tart의 delete(_ name: RemoteName) → removeItem + gc() 대응.
func (s *VMStorageOCI) Delete(name *RemoteName) error {
	if err := os.RemoveAll(s.vmURL(name)); err != nil {
		return err
	}
	return s.GC()
}

// Link는 태그 이미지를 다이제스트 이미지로 심볼릭 링크한다.
// tart의 link(from: RemoteName, to: RemoteName) 대응:
//   태그 경로 → 다이제스트 경로로 symlink 생성.
func (s *VMStorageOCI) Link(from, to *RemoteName) error {
	fromPath := s.vmURL(from)
	toPath := s.vmURL(to)

	// 기존 링크 삭제 (tart: try? FileManager.default.removeItem)
	os.RemoveAll(fromPath)
	os.MkdirAll(filepath.Dir(fromPath), 0755)

	// 심볼릭 링크 생성 (tart: createSymbolicLink(at:withDestinationURL:))
	if err := os.Symlink(toPath, fromPath); err != nil {
		return fmt.Errorf("심볼릭 링크 생성 실패: %w", err)
	}

	return s.GC()
}

// Linked는 from이 to를 가리키는 심볼릭 링크인지 확인한다.
// tart의 linked(from:to:) 대응.
func (s *VMStorageOCI) Linked(from, to *RemoteName) bool {
	fromPath := s.vmURL(from)
	target, err := os.Readlink(fromPath)
	if err != nil {
		return false
	}
	return target == s.vmURL(to)
}

// Digest는 심볼릭 링크의 실제 대상에서 다이제스트를 추출한다.
// tart의 digest(_ name: RemoteName) → resolvingSymlinksInPath().lastPathComponent.
func (s *VMStorageOCI) Digest(name *RemoteName) (string, error) {
	path := s.vmURL(name)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	last := filepath.Base(resolved)
	if !strings.HasPrefix(last, "sha256:") {
		return "", fmt.Errorf("%s는 다이제스트가 아니고 다이제스트를 가리키지도 않습니다", name)
	}
	return last, nil
}

// GC는 참조 카운트 기반 가비지 컬렉션을 수행한다.
// tart의 gc() 메서드를 정확히 재현:
//
// 알고리즘:
//   1) 모든 엔트리를 순회 (os.Lstat로 심볼릭 링크 감지)
//   2) 깨진 심볼릭 링크(대상이 없는) 삭제
//   3) 초기화된 VM 디렉토리의 참조 카운트 집계
//      - 심볼릭 링크: refCount += 1
//      - 실제 디렉토리: refCount += 0 (자기 자신만으로는 참조가 아님)
//   4) refCount == 0 이고 explicitlyPulled가 아닌 다이제스트 삭제
func (s *VMStorageOCI) GC() error {
	refCounts := make(map[string]int) // resolved path → 참조 카운트

	// 재귀적으로 모든 엔트리 순회 (tart: FileManager.enumerator)
	// filepath.Walk는 심볼릭 링크를 따라가므로, 직접 재귀 탐색한다.
	allPaths, err := collectAllPaths(s.BaseURL)
	if err != nil {
		return err
	}

	for _, path := range allPaths {
		// os.Lstat: 심볼릭 링크 자체의 정보를 반환 (따라가지 않음)
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}

		isSymlink := info.Mode()&os.ModeSymlink != 0

		if isSymlink {
			// 깨진 심볼릭 링크 감지 (tart: foundURL == foundURL.resolvingSymlinksInPath())
			target, linkErr := os.Readlink(path)
			if linkErr != nil {
				continue
			}
			if _, statErr := os.Stat(target); statErr != nil {
				// 대상이 없는 깨진 링크 삭제
				fmt.Printf("  [GC] 깨진 심볼릭 링크 삭제: %s\n", relPath(s.BaseURL, path))
				os.Remove(path)
				continue
			}
		}

		// 심볼릭 링크가 가리키는 실제 경로 확인
		// 주의: macOS에서 EvalSymlinks는 /var → /private/var 등을 해제하므로
		// 모든 경로를 EvalSymlinks로 정규화해야 동일 경로가 같은 키가 된다.
		resolvedPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}

		vmDir := &VMDirectory{BaseURL: resolvedPath}
		if !vmDir.Initialized() {
			continue
		}

		// 참조 카운트 집계 (tart: isSymlink ? 1 : 0)
		if _, ok := refCounts[resolvedPath]; !ok {
			refCounts[resolvedPath] = 0
		}
		if isSymlink {
			refCounts[resolvedPath]++
		}
	}

	// 참조되지 않는 다이제스트 삭제 (tart: !isExplicitlyPulled && incRefCount == 0)
	for path, count := range refCounts {
		vmDir := &VMDirectory{BaseURL: path}
		if !vmDir.IsExplicitlyPulled() && count == 0 {
			fmt.Printf("  [GC] 참조되지 않는 다이제스트 삭제: %s\n", relPath(s.BaseURL, path))
			os.RemoveAll(path)
		}
	}

	return nil
}

// collectAllPaths는 디렉토리 내 모든 경로를 재귀적으로 수집한다.
// 심볼릭 링크를 따라가지 않고 os.Lstat으로 판별하기 위해 직접 구현.
func collectAllPaths(root string) ([]string, error) {
	var paths []string

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		paths = append(paths, path)

		// 심볼릭 링크가 아닌 디렉토리만 재귀 탐색
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			subPaths, err := collectAllPaths(path)
			if err != nil {
				continue
			}
			paths = append(paths, subPaths...)
		}
	}

	return paths, nil
}

// List는 캐시된 모든 OCI 이미지를 나열한다.
// tart의 list() → enumerator + symlink 판별 + ":" 또는 "@" 구분자 사용.
func (s *VMStorageOCI) List() ([]struct {
	Name      string
	Dir       *VMDirectory
	IsSymlink bool
}, error) {
	var result []struct {
		Name      string
		Dir       *VMDirectory
		IsSymlink bool
	}

	allPaths, err := collectAllPaths(s.BaseURL)
	if err != nil {
		return nil, err
	}

	for _, path := range allPaths {
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}

		isSymlink := info.Mode()&os.ModeSymlink != 0

		resolvedPath := path
		if isSymlink {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				continue
			}
			resolvedPath = resolved
		}

		vmDir := &VMDirectory{BaseURL: resolvedPath}
		if !vmDir.Initialized() {
			continue
		}

		// tart: 태그는 ":", 다이제스트는 "@"로 조인
		rel, _ := filepath.Rel(s.BaseURL, path)
		dir := filepath.Dir(rel)
		base := filepath.Base(rel)

		sep := ":"
		if !isSymlink {
			sep = "@"
		}
		name := dir + sep + base

		result = append(result, struct {
			Name      string
			Dir       *VMDirectory
			IsSymlink bool
		}{name, vmDir, isSymlink})
	}

	return result, nil
}

// --- VMStorageHelper: Local/OCI 라우팅 (tart VMStorageHelper.swift 참조) ---

// VMStorageHelper는 VM 이름 형식에 따라 Local 또는 OCI 스토리지로 라우팅한다.
// tart의 VMStorageHelper.open/delete: RemoteName 파싱 가능 → OCI, 아니면 → Local
type VMStorageHelper struct {
	Local *VMStorageLocal
	OCI   *VMStorageOCI
}

// Open은 이름을 분석하여 적절한 스토리지에서 VM을 연다.
// tart: if let remoteName = try? RemoteName(name) → OCI, else → Local
func (h *VMStorageHelper) Open(name string) (*VMDirectory, error) {
	if remoteName, err := ParseRemoteName(name); err == nil {
		return h.OCI.Open(remoteName)
	}
	return h.Local.Open(name)
}

// Delete는 이름을 분석하여 적절한 스토리지에서 VM을 삭제한다.
func (h *VMStorageHelper) Delete(name string) error {
	if remoteName, err := ParseRemoteName(name); err == nil {
		return h.OCI.Delete(remoteName)
	}
	return h.Local.Delete(name)
}

// --- 유틸리티 함수 ---

// relPath는 base 기준 상대 경로를 반환한다 (출력 가독성용).
func relPath(base, path string) string {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return path
	}
	return rel
}

// =============================================================================
// 메인 함수: 전체 스토리지 워크플로우 시연
// =============================================================================

func main() {
	fmt.Println("=== Local/OCI 이중 스토리지 시뮬레이션 ===")
	fmt.Println("(tart VMStorageLocal.swift, VMStorageOCI.swift, VMStorageHelper.swift 기반)")
	fmt.Println()

	// 임시 디렉토리를 ~/.tart로 사용
	baseDir, err := os.MkdirTemp("", "tart-storage-poc")
	if err != nil {
		fmt.Printf("임시 디렉토리 생성 실패: %v\n", err)
		return
	}
	defer os.RemoveAll(baseDir)
	fmt.Printf("[설정] 시뮬레이션 디렉토리: %s\n\n", baseDir)

	// --- 1. VMStorageLocal 테스트 ---
	fmt.Println("========== 1. VMStorageLocal (로컬 VM 관리) ==========")
	fmt.Println()

	localStorage := NewVMStorageLocal(baseDir)
	fmt.Printf("[Local] 기본 경로: %s\n\n", localStorage.BaseURL)

	// VM 생성
	fmt.Println("[Local] VM 'macos-sonoma' 생성")
	vmDir1, err := localStorage.Create("macos-sonoma", false)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	fmt.Printf("  경로: %s\n", vmDir1.BaseURL)
	fmt.Printf("  초기화됨: %v\n\n", vmDir1.Initialized())

	fmt.Println("[Local] VM 'ubuntu-22' 생성")
	vmDir2, err := localStorage.Create("ubuntu-22", false)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	fmt.Printf("  경로: %s\n\n", vmDir2.BaseURL)

	// 존재 여부 확인
	fmt.Println("[Local] VM 존재 확인:")
	fmt.Printf("  'macos-sonoma': %v\n", localStorage.Exists("macos-sonoma"))
	fmt.Printf("  'ubuntu-22': %v\n", localStorage.Exists("ubuntu-22"))
	fmt.Printf("  'nonexistent': %v\n\n", localStorage.Exists("nonexistent"))

	// VM 열기
	fmt.Println("[Local] VM 'macos-sonoma' 열기")
	opened, err := localStorage.Open("macos-sonoma")
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	fmt.Printf("  이름: %s\n\n", opened.Name())

	// VM 목록
	fmt.Println("[Local] 전체 VM 목록:")
	vms, _ := localStorage.List()
	for _, vm := range vms {
		fmt.Printf("  - %s (%s)\n", vm.Name, vm.Dir.BaseURL)
	}
	fmt.Println()

	// VM 이름 변경
	fmt.Println("[Local] VM 'ubuntu-22' → 'ubuntu-24' 이름 변경")
	if err := localStorage.Rename("ubuntu-22", "ubuntu-24"); err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	fmt.Printf("  'ubuntu-22' 존재: %v\n", localStorage.Exists("ubuntu-22"))
	fmt.Printf("  'ubuntu-24' 존재: %v\n\n", localStorage.Exists("ubuntu-24"))

	// --- 2. VMStorageOCI 테스트 ---
	fmt.Println("========== 2. VMStorageOCI (OCI 캐시 관리) ==========")
	fmt.Println()

	ociStorage := NewVMStorageOCI(baseDir)
	fmt.Printf("[OCI] 기본 경로: %s\n\n", ociStorage.BaseURL)

	// 다이제스트 이미지 생성 (실제 Pull된 데이터)
	digestName1 := &RemoteName{
		Host:      "ghcr.io",
		Namespace: "cirruslabs/macos-sonoma-base",
		Reference: "sha256:abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
	}
	fmt.Printf("[OCI] 다이제스트 이미지 생성: %s\n", digestName1)
	vmDirOCI1, err := ociStorage.Create(digestName1, false)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	fmt.Printf("  경로: %s\n\n", vmDirOCI1.BaseURL)

	// 태그 → 다이제스트 심볼릭 링크
	tagName1 := &RemoteName{
		Host:      "ghcr.io",
		Namespace: "cirruslabs/macos-sonoma-base",
		Reference: "latest",
	}
	fmt.Printf("[OCI] 태그 '%s' → 다이제스트 링크 생성\n", tagName1)
	if err := ociStorage.Link(tagName1, digestName1); err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	fmt.Printf("  링크 확인: %v\n", ociStorage.Linked(tagName1, digestName1))

	// 다이제스트 추출
	digest, err := ociStorage.Digest(tagName1)
	if err != nil {
		fmt.Printf("  다이제스트 추출 오류: %v\n", err)
	} else {
		fmt.Printf("  추출된 다이제스트: %s\n", digest)
	}
	fmt.Println()

	// 두 번째 이미지 추가 (다른 다이제스트)
	digestName2 := &RemoteName{
		Host:      "ghcr.io",
		Namespace: "cirruslabs/macos-sonoma-base",
		Reference: "sha256:9876543210fedcba9876543210fedcba9876543210fedcba9876543210fedcba",
	}
	fmt.Printf("[OCI] 새 다이제스트 이미지 생성: %s\n", digestName2)
	_, err = ociStorage.Create(digestName2, false)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}

	// 태그를 새 다이제스트로 재연결
	fmt.Printf("[OCI] 태그 'latest' → 새 다이제스트로 재연결\n")
	if err := ociStorage.Link(tagName1, digestName2); err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	fmt.Printf("  이전 다이제스트 링크: %v\n", ociStorage.Linked(tagName1, digestName1))
	fmt.Printf("  새 다이제스트 링크: %v\n\n", ociStorage.Linked(tagName1, digestName2))

	// --- 3. GC (가비지 컬렉션) ---
	fmt.Println("========== 3. 가비지 컬렉션 ==========")
	fmt.Println()
	fmt.Println("[GC] 현재 상태:")
	fmt.Println("  - 다이제스트1 (이전): 태그 참조 없음 → 삭제 대상")
	fmt.Println("  - 다이제스트2 (현재): latest 태그가 참조 → 유지")
	fmt.Println()

	fmt.Println("[GC] GC 실행:")
	if err := ociStorage.GC(); err != nil {
		fmt.Printf("  오류: %v\n", err)
	}
	fmt.Println()

	fmt.Printf("[GC] 다이제스트1 존재: %v (삭제됨)\n", ociStorage.Exists(digestName1))
	fmt.Printf("[GC] 다이제스트2 존재: %v (유지됨)\n\n", ociStorage.Exists(digestName2))

	// --- 4. ExplicitlyPulled 테스트 ---
	fmt.Println("========== 4. ExplicitlyPulled 테스트 ==========")
	fmt.Println()

	digestName3 := &RemoteName{
		Host:      "ghcr.io",
		Namespace: "cirruslabs/macos-ventura-base",
		Reference: "sha256:1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff",
	}
	fmt.Printf("[OCI] 다이제스트로 직접 Pull된 이미지 생성: %s\n", digestName3)
	vmDirOCI3, err := ociStorage.Create(digestName3, false)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	// tart: digestName으로 직접 Pull하면 markExplicitlyPulled() 호출
	vmDirOCI3.MarkExplicitlyPulled()
	fmt.Printf("  ExplicitlyPulled: %v\n\n", vmDirOCI3.IsExplicitlyPulled())

	fmt.Println("[GC] GC 실행 (ExplicitlyPulled 이미지는 참조 없어도 유지):")
	if err := ociStorage.GC(); err != nil {
		fmt.Printf("  오류: %v\n", err)
	}
	fmt.Printf("[GC] 다이제스트3 존재: %v (ExplicitlyPulled → 유지)\n\n", ociStorage.Exists(digestName3))

	// --- 5. VMStorageHelper 테스트 ---
	fmt.Println("========== 5. VMStorageHelper (라우팅) ==========")
	fmt.Println()

	helper := &VMStorageHelper{
		Local: localStorage,
		OCI:   ociStorage,
	}

	// 로컬 이름 → VMStorageLocal
	fmt.Println("[Helper] 'macos-sonoma' 열기 (로컬 이름 → VMStorageLocal)")
	vmDir, err := helper.Open("macos-sonoma")
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Printf("  이름: %s, 경로: %s\n\n", vmDir.Name(), vmDir.BaseURL)
	}

	// 원격 이름 → VMStorageOCI
	fmt.Println("[Helper] 'ghcr.io/cirruslabs/macos-sonoma-base:latest' 열기 (원격 이름 → VMStorageOCI)")
	vmDir, err = helper.Open("ghcr.io/cirruslabs/macos-sonoma-base:latest")
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Printf("  이름: %s, 경로: %s\n\n", vmDir.Name(), vmDir.BaseURL)
	}

	// 존재하지 않는 VM
	fmt.Println("[Helper] 'nonexistent' 열기 (존재하지 않는 VM)")
	_, err = helper.Open("nonexistent")
	if err != nil {
		fmt.Printf("  오류: %v\n\n", err)
	}

	// --- 6. OCI 이미지 목록 ---
	fmt.Println("========== 6. OCI 캐시 이미지 목록 ==========")
	fmt.Println()
	ociList, _ := ociStorage.List()
	for _, item := range ociList {
		linkStr := "디렉토리"
		if item.IsSymlink {
			linkStr = "심볼릭 링크"
		}
		fmt.Printf("  [%s] %s\n", linkStr, item.Name)
	}
	fmt.Println()

	// --- 7. 정리 ---
	fmt.Println("========== 7. 삭제 및 정리 ==========")
	fmt.Println()

	fmt.Println("[Local] VM 'macos-sonoma' 삭제")
	localStorage.Delete("macos-sonoma")
	fmt.Printf("  존재: %v\n\n", localStorage.Exists("macos-sonoma"))

	fmt.Println("[Helper] OCI 이미지 삭제: 'ghcr.io/cirruslabs/macos-sonoma-base:latest'")
	helper.Delete("ghcr.io/cirruslabs/macos-sonoma-base:latest")
	fmt.Println()

	// --- 요약 ---
	fmt.Println("========== 요약 ==========")
	fmt.Println()
	fmt.Println("VMStorageLocal (tart VMStorageLocal.swift):")
	fmt.Println("  - 파일 시스템 디렉토리 기반 VM 관리")
	fmt.Println("  - ~/.tart/vms/{name}/ 구조")
	fmt.Println("  - Create, Open, Rename, Delete, List 지원")
	fmt.Println()
	fmt.Println("VMStorageOCI (tart VMStorageOCI.swift):")
	fmt.Println("  - 심볼릭 링크 기반 OCI 이미지 캐시")
	fmt.Println("  - 태그 → 다이제스트 심볼릭 링크 관리")
	fmt.Println("  - 참조 카운트 기반 GC (unreferenced 다이제스트 삭제)")
	fmt.Println("  - ExplicitlyPulled 이미지는 GC에서 보호")
	fmt.Println()
	fmt.Println("VMStorageHelper (tart VMStorageHelper.swift):")
	fmt.Println("  - 이름 형식 분석 → Local 또는 OCI 스토리지 라우팅")
	fmt.Println("  - RemoteName 파싱 가능 → OCI, 아니면 → Local")
	fmt.Println()
	fmt.Println("[완료] Local/OCI 이중 스토리지 시뮬레이션 성공")
}
