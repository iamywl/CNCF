# PoC-14: APFS Copy-on-Write 클론 시뮬레이션

## 개요

Tart의 `VMDirectory.clone()` 메서드와 `Clone.swift`의 APFS Copy-on-Write 클론 메커니즘을 Go로 재현한다. 블록 레벨 참조 카운팅, CoW 쓰기 분리, sizeBytes vs allocatedSizeBytes 차이, MAC 주소 재생성, 자동 프루닝 연계를 시뮬레이션한다.

## Tart 소스코드 매핑

| Tart 소스 | PoC 대응 | 설명 |
|-----------|---------|------|
| `VMDirectory.swift` — `clone(to:generateMAC:)` | `VMDirectory.Clone()` | APFS CoW 클론 수행 |
| `FileManager.copyItem(at:to:)` | `VMFile.Clone()` (참조 카운트 증가) | APFS clonefile() 시뮬레이션 |
| `VMDirectory.sizeBytes()` | `VMDirectory.SizeBytes()` | 논리적 전체 크기 |
| `VMDirectory.allocatedSizeBytes()` | `VMDirectory.AllocatedSizeBytes()` | 물리적 디스크 사용량 |
| `Clone.swift` — `sourceVM.clone(to: tmpVMDir, ...)` | 데모 6 흐름 | 전체 clone 명령 흐름 |
| `VMDirectory.regenerateMACAddress()` | `generateRandomMAC()` | 네트워크 충돌 방지 |
| `sizeBytes() - allocatedSizeBytes()` | 미할당 공간 계산 | CoW 미할당 공간 확보 |

## 핵심 개념

### 1. APFS Copy-on-Write

```
원본 VM          클론 VM
┌─────────┐      ┌─────────┐
│ 블록 A ──│──┐   │──┘       │   블록 A (RefCount=2)
│ 블록 B ──│──│───│──┘       │   블록 B (RefCount=2)
│ 블록 C ──│──│───│──┘       │   블록 C (RefCount=2)
└─────────┘  │   └─────────┘
             │
             └── 모든 블록이 공유됨 (물리적 복사 없음)

쓰기 발생 시:
원본 VM          클론 VM
┌─────────┐      ┌─────────┐
│ 블록 A ──│──┐   │          │   블록 A (RefCount=1, 원본만)
│ 블록 B ──│──│───│──┘       │   블록 B (RefCount=2, 공유)
│ 블록 C ──│──│───│──┘       │   블록 C (RefCount=2, 공유)
└─────────┘  │   │ 블록 A' ──│   블록 A' (RefCount=1, 클론만, 새 데이터)
             │   └─────────┘
```

### 2. Clone.swift 전체 흐름

```
Clone.run()
├── 원격 이미지면 pull
├── VMStorageHelper.open(sourceName)
├── VMDirectory.temporary() — 임시 디렉토리
├── FileLock(tmpVMDir) — GC 방지
├── FileLock(tartHomeDir) — 전역 잠금
├── sourceVM.clone(to: tmpVMDir, generateMAC:)
│   ├── copyItem(configURL) — clonefile()
│   ├── copyItem(nvramURL) — clonefile()
│   ├── copyItem(diskURL) — clonefile()
│   └── regenerateMACAddress() (충돌 시)
├── localStorage.move(newName)
├── 전역 잠금 해제
└── Prune.reclaimIfNeeded(unallocatedBytes)
```

### 3. sizeBytes vs allocatedSizeBytes

| 측정값 | 의미 | CoW 클론 직후 |
|--------|------|--------------|
| sizeBytes | 논리적 전체 크기 | 원본과 동일 |
| allocatedSizeBytes | 실제 디스크 사용량 | 거의 0 (공유) |
| unallocatedBytes | sizeBytes - allocatedSizeBytes | 실행 시 필요할 공간 |

## 실행 방법

```bash
cd poc-14-apfs-clone
go run main.go
```

## 학습 포인트

1. **clonefile()**: 블록 복사 없이 메타데이터만 복제하여 즉각적 완료
2. **참조 카운팅**: 블록별 참조 수 추적으로 안전한 해제 보장
3. **Copy-on-Write**: 쓰기 시에만 새 블록을 할당하여 디스크 절약
4. **미할당 공간 관리**: CoW 클론의 미할당 공간을 미리 확보하여 런타임 에러 방지
5. **MAC 주소 재생성**: 같은 네트워크에서 VM 충돌 방지
