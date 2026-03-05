package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// =============================================================================
// PoC-13: 파일 잠금 메커니즘 시뮬레이션
// =============================================================================
// Tart의 FileLock.swift와 PIDLock.swift의 잠금 구조를 Go로 재현한다.
// 핵심 개념:
//   - FileLock: flock() 기반 권고적(advisory) 파일 잠금
//     - LOCK_EX: 배타적 잠금 (읽기/쓰기 모두 차단)
//     - LOCK_NB: 비차단 모드 (잠금 실패 시 즉시 반환)
//     - LOCK_UN: 잠금 해제
//     - EWOULDBLOCK: 이미 잠겨있을 때 비차단 모드에서 반환
//   - PIDLock: fcntl() 기반 잠금 + PID 추적
//     - F_SETLK: 비차단 잠금 시도
//     - F_SETLKW: 차단 잠금 (대기)
//     - F_GETLK: 현재 잠금 상태 조회 (어떤 PID가 잠고 있는지)
//     - F_RDLCK/F_WRLCK/F_UNLCK: 읽기/쓰기/해제 잠금 타입
//   - 사용처: VM 실행 중 잠금, OCI pull 동시 접근 방지, clone 전역 잠금
//
// 실제 소스: Sources/tart/FileLock.swift, Sources/tart/PIDLock.swift
// =============================================================================

// ---------------------------------------------------------------------------
// 1. FileLock — Sources/tart/FileLock.swift 참조
// ---------------------------------------------------------------------------

// FileLock은 flock() 기반 파일 잠금을 시뮬레이션한다.
// Tart에서는 OCI pull 시 호스트 디렉토리에 대한 동시 접근을 방지하는 데 사용한다.
//
// FileLock.swift:
//   class FileLock {
//     let url: URL
//     let fd: Int32
//     init(lockURL: URL) throws {
//       url = lockURL
//       fd = open(lockURL.path, 0)
//     }
//     func trylock() throws -> Bool { try flockWrapper(LOCK_EX | LOCK_NB) }
//     func lock() throws { _ = try flockWrapper(LOCK_EX) }
//     func unlock() throws { _ = try flockWrapper(LOCK_UN) }
//   }
type FileLock struct {
	path string
	fd   int
}

// NewFileLock은 파일 잠금 객체를 생성한다.
// Tart: init(lockURL: URL) throws { fd = open(lockURL.path, 0) }
func NewFileLock(path string) (*FileLock, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("잠금 파일 열기 실패 %s: %w", path, err)
	}
	return &FileLock{path: path, fd: fd}, nil
}

// TryLock은 비차단 배타적 잠금을 시도한다.
// 성공 시 true, 이미 잠겨있으면 false를 반환한다 (EWOULDBLOCK).
// Tart: func trylock() throws -> Bool { try flockWrapper(LOCK_EX | LOCK_NB) }
func (fl *FileLock) TryLock() (bool, error) {
	err := syscall.Flock(fl.fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		// EWOULDBLOCK = 이미 다른 프로세스가 잠금을 보유 중
		if err == syscall.EWOULDBLOCK {
			return false, nil
		}
		return false, fmt.Errorf("flock 실패: %w", err)
	}
	return true, nil
}

// Lock은 차단 배타적 잠금을 수행한다. 잠금을 획득할 때까지 대기한다.
// Tart: func lock() throws { _ = try flockWrapper(LOCK_EX) }
func (fl *FileLock) Lock() error {
	err := syscall.Flock(fl.fd, syscall.LOCK_EX)
	if err != nil {
		return fmt.Errorf("flock 차단 잠금 실패: %w", err)
	}
	return nil
}

// Unlock은 잠금을 해제한다.
// Tart: func unlock() throws { _ = try flockWrapper(LOCK_UN) }
func (fl *FileLock) Unlock() error {
	err := syscall.Flock(fl.fd, syscall.LOCK_UN)
	if err != nil {
		return fmt.Errorf("flock 해제 실패: %w", err)
	}
	return nil
}

// Close는 파일 디스크립터를 닫는다.
// Tart: deinit { close(fd) }
func (fl *FileLock) Close() {
	syscall.Close(fl.fd)
}

// ---------------------------------------------------------------------------
// 2. PIDLock — Sources/tart/PIDLock.swift 참조
// ---------------------------------------------------------------------------

// PIDLock은 fcntl() 기반 잠금으로, 잠금을 보유한 프로세스의 PID를 추적할 수 있다.
// Tart에서는 VM이 실행 중인지 확인하는 데 사용한다.
// config.json 파일에 fcntl 잠금을 걸고, 어떤 PID가 잠금을 보유하는지 조회한다.
//
// PIDLock.swift:
//   class PIDLock {
//     let fd: Int32
//     init(lockURL: URL) throws { fd = open(lockURL.path, O_RDWR) }
//     func trylock() throws -> Bool { lockWrapper(F_SETLK, F_WRLCK, ...) }
//     func lock() throws { lockWrapper(F_SETLKW, F_WRLCK, ...) }
//     func unlock() throws { lockWrapper(F_SETLK, F_UNLCK, ...) }
//     func pid() throws -> pid_t { lockWrapper(F_GETLK, F_RDLCK, ...).l_pid }
//   }
type PIDLock struct {
	path string
	fd   int
}

// NewPIDLock은 PID 잠금 객체를 생성한다.
// Tart: init(lockURL: URL) throws { fd = open(lockURL.path, O_RDWR) }
func NewPIDLock(path string) (*PIDLock, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("PID 잠금 파일 열기 실패 %s: %w", path, err)
	}
	return &PIDLock{path: path, fd: fd}, nil
}

// fcntlFlock은 fcntl F_SETLK/F_SETLKW/F_GETLK 호출을 래핑한다.
// Tart: func lockWrapper(_ operation: Int32, _ type: Int32, ...) throws -> (Bool, flock)
func (pl *PIDLock) fcntlFlock(cmd int, lockType int16) (bool, *syscall.Flock_t, error) {
	flock := &syscall.Flock_t{
		Start:  0,
		Len:    0, // 전체 파일
		Pid:    0,
		Type:   lockType,
		Whence: int16(os.SEEK_SET),
	}

	err := syscall.FcntlFlock(uintptr(pl.fd), cmd, flock)
	if err != nil {
		// F_SETLK에서 이미 잠겨있으면 EAGAIN/EACCES
		if cmd == syscall.F_SETLK && (err == syscall.EAGAIN || err == syscall.EACCES) {
			return false, flock, nil
		}
		return false, nil, fmt.Errorf("fcntl 실패: %w", err)
	}

	return true, flock, nil
}

// TryLock은 비차단 쓰기 잠금을 시도한다.
// Tart: func trylock() throws -> Bool { lockWrapper(F_SETLK, F_WRLCK, ...) }
func (pl *PIDLock) TryLock() (bool, error) {
	ok, _, err := pl.fcntlFlock(syscall.F_SETLK, syscall.F_WRLCK)
	return ok, err
}

// Lock은 차단 쓰기 잠금을 수행한다 (대기).
// Tart: func lock() throws { lockWrapper(F_SETLKW, F_WRLCK, ...) }
func (pl *PIDLock) Lock() error {
	_, _, err := pl.fcntlFlock(syscall.F_SETLKW, syscall.F_WRLCK)
	return err
}

// Unlock은 잠금을 해제한다.
// Tart: func unlock() throws { lockWrapper(F_SETLK, F_UNLCK, ...) }
func (pl *PIDLock) Unlock() error {
	_, _, err := pl.fcntlFlock(syscall.F_SETLK, syscall.F_UNLCK)
	return err
}

// GetLockPID는 현재 잠금을 보유한 PID를 반환한다.
// 잠금이 없으면 PID 0을 반환한다.
// Tart: func pid() throws -> pid_t { lockWrapper(F_GETLK, F_RDLCK, ...).l_pid }
//
// VMDirectory.running()에서 사용:
//   func running() throws -> Bool {
//     guard let lock = try? lock() else { return false }
//     return try lock.pid() != 0
//   }
func (pl *PIDLock) GetLockPID() (int32, error) {
	_, flock, err := pl.fcntlFlock(syscall.F_GETLK, syscall.F_RDLCK)
	if err != nil {
		return 0, err
	}
	// F_GETLK: 잠금이 설정되어 있으면 l_pid에 소유 PID, 없으면 F_UNLCK로 변경됨
	if flock.Type == syscall.F_UNLCK {
		return 0, nil // 잠금 없음
	}
	return flock.Pid, nil
}

// Close는 파일 디스크립터를 닫는다.
func (pl *PIDLock) Close() {
	syscall.Close(pl.fd)
}

// ---------------------------------------------------------------------------
// 3. VMDirectory 잠금 시뮬레이션
// ---------------------------------------------------------------------------

// VMDirectorySim은 Tart의 VMDirectory에서 잠금 관련 부분만 시뮬레이션한다.
// VMDirectory.swift:
//   func lock() throws -> PIDLock { try PIDLock(lockURL: configURL) }
//   func running() throws -> Bool {
//     guard let lock = try? lock() else { return false }
//     return try lock.pid() != 0
//   }
//   func delete() throws {
//     let lock = try lock()
//     if try !lock.trylock() { throw RuntimeError.VMIsRunning(name) }
//     try FileManager.default.removeItem(at: baseURL)
//     try lock.unlock()
//   }
type VMDirectorySim struct {
	basePath   string
	configPath string // config.json — PIDLock 대상
}

func NewVMDirectorySim(basePath string) (*VMDirectorySim, error) {
	configPath := filepath.Join(basePath, "config.json")

	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, err
	}
	f, err := os.Create(configPath)
	if err != nil {
		return nil, err
	}
	f.WriteString(`{"version":1}`)
	f.Close()

	return &VMDirectorySim{
		basePath:   basePath,
		configPath: configPath,
	}, nil
}

// Lock은 VM 디렉토리에 PIDLock을 생성한다.
func (vd *VMDirectorySim) Lock() (*PIDLock, error) {
	return NewPIDLock(vd.configPath)
}

// IsRunning은 VM이 실행 중인지 확인한다.
func (vd *VMDirectorySim) IsRunning() bool {
	lock, err := vd.Lock()
	if err != nil {
		return false
	}
	defer lock.Close()

	pid, err := lock.GetLockPID()
	if err != nil {
		return false
	}
	return pid != 0
}

// TryDelete은 VM이 실행 중이 아니면 삭제한다.
func (vd *VMDirectorySim) TryDelete() error {
	lock, err := vd.Lock()
	if err != nil {
		return err
	}

	ok, err := lock.TryLock()
	if err != nil {
		lock.Close()
		return err
	}
	if !ok {
		lock.Close()
		return fmt.Errorf("VM이 실행 중이므로 삭제할 수 없습니다: %s",
			filepath.Base(vd.basePath))
	}

	fmt.Printf("  VM 디렉토리 삭제: %s\n", vd.basePath)
	lock.Unlock()
	lock.Close()
	return os.RemoveAll(vd.basePath)
}

// ---------------------------------------------------------------------------
// 4. 출력 헬퍼
// ---------------------------------------------------------------------------

func printSeparator(title string) {
	fmt.Printf("\n--- %s ---\n", title)
}

// ---------------------------------------------------------------------------
// 5. 메인 함수
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("=== PoC-13: 파일 잠금 메커니즘 시뮬레이션 ===")
	fmt.Println("  소스: FileLock.swift (flock 기반), PIDLock.swift (fcntl 기반)")
	fmt.Println()

	// 임시 디렉토리 생성
	tmpDir, err := os.MkdirTemp("", "tart-poc-13-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "임시 디렉토리 생성 실패: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	// =========================================================================
	// 데모 1: FileLock — flock() 기반 배타적 잠금
	// =========================================================================
	printSeparator("데모 1: FileLock -- flock() 기반 배타적 잠금")

	lockFilePath := filepath.Join(tmpDir, "host-dir")
	if err := os.MkdirAll(lockFilePath, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "디렉토리 생성 실패: %v\n", err)
		os.Exit(1)
	}

	lock1, err := NewFileLock(lockFilePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FileLock 생성 실패: %v\n", err)
		os.Exit(1)
	}

	// 1-1. TryLock 성공
	ok, err := lock1.TryLock()
	if err != nil {
		fmt.Printf("  TryLock 오류: %v\n", err)
	} else {
		fmt.Printf("  첫 번째 TryLock: %v (성공)\n", ok)
	}

	// 1-2. 같은 프로세스에서 두 번째 TryLock
	lock2, _ := NewFileLock(lockFilePath)
	ok2, err := lock2.TryLock()
	fmt.Printf("  같은 프로세스 두 번째 TryLock: %v (flock은 프로세스 내 재진입 허용)\n", ok2)
	if err != nil {
		fmt.Printf("    오류: %v\n", err)
	}

	// 1-3. 잠금 해제
	lock1.Unlock()
	fmt.Println("  잠금 해제 완료")
	lock1.Close()
	lock2.Close()

	// =========================================================================
	// 데모 2: FileLock — 동시 접근 시뮬레이션 (goroutine 기반)
	// =========================================================================
	printSeparator("데모 2: FileLock -- 동시 접근 시뮬레이션")

	fmt.Println("  Tart OCI pull 시 호스트 디렉토리 잠금 패턴:")
	fmt.Println("    let lock = try FileLock(lockURL: hostDirectoryURL)")
	fmt.Println("    let successfullyLocked = try lock.trylock()")
	fmt.Println("    if !successfullyLocked {")
	fmt.Println("      print(\"waiting for lock...\")")
	fmt.Println("      try lock.lock()  // 차단 대기")
	fmt.Println("    }")
	fmt.Println()

	sharedFile := filepath.Join(tmpDir, "shared-resource")
	os.Create(sharedFile)

	var wg sync.WaitGroup
	for i := 1; i <= 3; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			fl, err := NewFileLock(sharedFile)
			if err != nil {
				fmt.Printf("  [Worker %d] FileLock 생성 실패: %v\n", id, err)
				return
			}
			defer fl.Close()

			ok, err := fl.TryLock()
			if err != nil {
				fmt.Printf("  [Worker %d] TryLock 오류: %v\n", id, err)
				return
			}

			if !ok {
				fmt.Printf("  [Worker %d] TryLock 실패 (EWOULDBLOCK) -> 차단 대기 중...\n", id)
				if err := fl.Lock(); err != nil {
					fmt.Printf("  [Worker %d] Lock 오류: %v\n", id, err)
					return
				}
			}

			fmt.Printf("  [Worker %d] 잠금 획득! 작업 수행 중...\n", id)
			time.Sleep(50 * time.Millisecond)

			fl.Unlock()
			fmt.Printf("  [Worker %d] 잠금 해제\n", id)
		}(i)
		time.Sleep(10 * time.Millisecond)
	}
	wg.Wait()

	// =========================================================================
	// 데모 3: PIDLock — fcntl() 기반 잠금 + PID 추적
	// =========================================================================
	printSeparator("데모 3: PIDLock -- fcntl() 기반 잠금 + PID 추적")

	configFile := filepath.Join(tmpDir, "config.json")
	f, _ := os.Create(configFile)
	f.WriteString(`{"version":1,"os":"darwin"}`)
	f.Close()

	pidLock, err := NewPIDLock(configFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "PIDLock 생성 실패: %v\n", err)
		os.Exit(1)
	}

	// 3-1. 잠금 전 PID 확인
	pid, _ := pidLock.GetLockPID()
	fmt.Printf("  잠금 전 PID: %d (0 = 잠금 없음)\n", pid)

	// 3-2. 잠금 획득
	ok, _ = pidLock.TryLock()
	fmt.Printf("  TryLock: %v\n", ok)

	// 3-3. 잠금 후 PID 확인 (다른 fd에서 조회)
	pidLock2, _ := NewPIDLock(configFile)
	pid2, _ := pidLock2.GetLockPID()
	fmt.Printf("  잠금 후 PID (다른 fd에서 조회): %d (현재 프로세스 PID: %d)\n", pid2, os.Getpid())
	pidLock2.Close()

	// 3-4. 잠금 해제
	pidLock.Unlock()
	fmt.Println("  잠금 해제 완료")

	pidLock3, _ := NewPIDLock(configFile)
	pid3, _ := pidLock3.GetLockPID()
	fmt.Printf("  해제 후 PID: %d (0 = 잠금 없음)\n", pid3)
	pidLock3.Close()
	pidLock.Close()

	// =========================================================================
	// 데모 4: VMDirectory 잠금 — VM 실행 상태 확인
	// =========================================================================
	printSeparator("데모 4: VMDirectory 잠금 -- VM 실행 상태 확인")

	vmPath := filepath.Join(tmpDir, "vms", "macos-sonoma")
	vmDir, err := NewVMDirectorySim(vmPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "VMDirectory 생성 실패: %v\n", err)
		os.Exit(1)
	}

	// 4-1. VM 미실행 상태
	fmt.Printf("  VM 실행 중: %v (잠금 없음)\n", vmDir.IsRunning())

	// 4-2. VM 실행 시뮬레이션 (PIDLock 획득)
	vmLock, _ := vmDir.Lock()
	vmLock.Lock()
	fmt.Println("  VM 잠금 획득 (실행 시작 시뮬레이션)")
	fmt.Printf("  VM 실행 중: %v\n", vmDir.IsRunning())

	// 4-3. 실행 중인 VM 삭제 시도 -> 실패
	err = vmDir.TryDelete()
	if err != nil {
		fmt.Printf("  삭제 시도: %v\n", err)
	}

	// 4-4. VM 종료 (잠금 해제)
	vmLock.Unlock()
	vmLock.Close()
	fmt.Println("  VM 잠금 해제 (종료 시뮬레이션)")
	fmt.Printf("  VM 실행 중: %v\n", vmDir.IsRunning())

	// 4-5. 종료 후 삭제 -> 성공
	vmDir2, _ := NewVMDirectorySim(vmPath)
	err = vmDir2.TryDelete()
	if err != nil {
		fmt.Printf("  삭제 결과: %v\n", err)
	} else {
		fmt.Println("  삭제 성공")
	}

	// =========================================================================
	// 데모 5: Tart에서의 잠금 사용 패턴 요약
	// =========================================================================
	printSeparator("데모 5: Tart 잠금 사용 패턴 요약")

	patterns := []struct {
		usage    string
		lockType string
		target   string
		purpose  string
	}{
		{"OCI Pull", "FileLock", "호스트 디렉토리", "동일 호스트 이미지 동시 pull 방지"},
		{"OCI Pull (임시)", "FileLock", "tmpVMDir.baseURL", "임시 디렉토리 GC 방지"},
		{"Clone", "FileLock", "tartHomeDir", "전역 clone 직렬화"},
		{"VM Run", "PIDLock", "config.json", "VM 실행 상태 추적 (PID)"},
		{"VM Delete", "PIDLock", "config.json", "실행 중 삭제 방지"},
		{"VMDirectory.running()", "PIDLock", "config.json", "VM 실행 여부 확인 (PID != 0)"},
	}

	fmt.Printf("  %-22s %-12s %-20s %s\n", "사용처", "잠금 타입", "대상", "목적")
	fmt.Printf("  %s\n", strings.Repeat("-", 85))
	for _, p := range patterns {
		fmt.Printf("  %-22s %-12s %-20s %s\n", p.usage, p.lockType, p.target, p.purpose)
	}

	// =========================================================================
	// 데모 6: flock vs fcntl 비교
	// =========================================================================
	printSeparator("데모 6: flock vs fcntl 비교")

	comparison := []struct {
		item  string
		flock string
		fcntl string
	}{
		{"시스템 콜", "flock(fd, operation)", "fcntl(fd, F_SETLK, &flock)"},
		{"잠금 단위", "전체 파일", "바이트 범위 (l_start, l_len)"},
		{"PID 추적", "불가", "F_GETLK로 l_pid 조회 가능"},
		{"프로세스 내 재진입", "허용 (같은 fd)", "허용 (같은 프로세스)"},
		{"Tart 사용", "FileLock.swift", "PIDLock.swift"},
		{"Tart 용도", "디렉토리 잠금", "VM 실행 상태 추적"},
	}

	fmt.Printf("  %-20s %-30s %-30s\n", "항목", "flock", "fcntl")
	fmt.Printf("  %s\n", strings.Repeat("-", 80))
	for _, c := range comparison {
		fmt.Printf("  %-20s %-30s %-30s\n", c.item, c.flock, c.fcntl)
	}

	fmt.Println()
	fmt.Println("  [설계 이유]")
	fmt.Println("  - FileLock (flock): 단순한 상호 배제 (OCI pull 동시 접근 방지)")
	fmt.Println("  - PIDLock (fcntl): PID 추적이 필요한 경우 (VM 실행 상태 확인)")
	fmt.Println("    -> F_GETLK로 어떤 프로세스가 VM을 실행 중인지 확인 가능")
	fmt.Println("    -> 프로세스가 비정상 종료해도 커널이 자동으로 잠금 해제")
}
