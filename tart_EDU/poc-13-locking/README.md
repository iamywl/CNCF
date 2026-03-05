# PoC-13: 파일 잠금 메커니즘 시뮬레이션

## 개요

Tart의 `FileLock.swift`(flock 기반)과 `PIDLock.swift`(fcntl 기반) 잠금 메커니즘을 Go로 재현한다. VM 실행 상태 추적, OCI pull 동시 접근 방지, clone 전역 잠금 등 Tart의 다양한 잠금 사용 패턴을 시뮬레이션한다.

## Tart 소스코드 매핑

| Tart 소스 | PoC 대응 | 설명 |
|-----------|---------|------|
| `FileLock.swift` — `class FileLock` | `FileLock` 구조체 | flock() 기반 파일 잠금 |
| `FileLock.trylock()` — `LOCK_EX \| LOCK_NB` | `FileLock.TryLock()` | 비차단 배타적 잠금 시도 |
| `FileLock.lock()` — `LOCK_EX` | `FileLock.Lock()` | 차단 배타적 잠금 |
| `FileLock.flockWrapper()` — EWOULDBLOCK 처리 | `TryLock()` 내 EWOULDBLOCK 분기 | 이미 잠겨있을 때 false 반환 |
| `PIDLock.swift` — `class PIDLock` | `PIDLock` 구조체 | fcntl() 기반 잠금 + PID 추적 |
| `PIDLock.pid()` — `F_GETLK, F_RDLCK` | `PIDLock.GetLockPID()` | 잠금 소유 PID 조회 |
| `VMDirectory.lock()` | `VMDirectorySim.Lock()` | config.json에 PIDLock 생성 |
| `VMDirectory.running()` — `lock.pid() != 0` | `VMDirectorySim.IsRunning()` | VM 실행 여부 확인 |
| `VMDirectory.delete()` — `!lock.trylock()` -> throw | `VMDirectorySim.TryDelete()` | 실행 중 삭제 방지 |

## 핵심 개념

### 1. FileLock (flock 기반)

```
flock(fd, LOCK_EX | LOCK_NB)  ->  성공: true, 실패(EWOULDBLOCK): false
flock(fd, LOCK_EX)             ->  차단 대기 (잠금 획득까지)
flock(fd, LOCK_UN)             ->  잠금 해제
```

Tart에서 OCI pull 시 동일 호스트 이미지의 동시 pull을 방지하는 데 사용한다:
```swift
let lock = try FileLock(lockURL: hostDirectoryURL)
let successfullyLocked = try lock.trylock()
if !successfullyLocked {
    print("waiting for lock...")
    try lock.lock()  // 차단 대기
}
```

### 2. PIDLock (fcntl 기반)

```
fcntl(fd, F_SETLK, F_WRLCK)   ->  비차단 쓰기 잠금 (EAGAIN 시 false)
fcntl(fd, F_SETLKW, F_WRLCK)  ->  차단 쓰기 잠금
fcntl(fd, F_GETLK, F_RDLCK)   ->  잠금 상태 조회 (l_pid로 소유 PID 확인)
fcntl(fd, F_SETLK, F_UNLCK)   ->  잠금 해제
```

### 3. flock vs fcntl 비교

| 항목 | flock (FileLock) | fcntl (PIDLock) |
|------|-----------------|-----------------|
| 잠금 단위 | 전체 파일 | 바이트 범위 |
| PID 추적 | 불가 | F_GETLK로 가능 |
| Tart 용도 | 디렉토리 잠금 (OCI pull/clone) | VM 실행 상태 추적 |
| 비정상 종료 시 | 커널 자동 해제 | 커널 자동 해제 |

## 실행 방법

```bash
cd poc-13-locking
go run main.go
```

## 학습 포인트

1. **권고적 잠금 (Advisory Lock)**: Unix에서 파일 잠금은 강제가 아닌 협약 기반
2. **flock vs fcntl**: 목적에 따라 적절한 잠금 메커니즘 선택
3. **PID 추적**: fcntl F_GETLK로 잠금 소유 프로세스를 식별하여 VM 상태 확인
4. **자동 해제**: 프로세스 종료 시 커널이 자동으로 잠금 해제 (좀비 잠금 방지)
5. **EWOULDBLOCK 처리**: TryLock 패턴에서 잠금 실패를 에러가 아닌 상태값으로 처리
