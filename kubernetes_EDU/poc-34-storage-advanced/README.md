# PoC-34: CSI Migration 및 Ephemeral Volume 심화 시뮬레이션

## 개요

이 PoC는 Kubernetes 스토리지 시스템의 두 가지 핵심 메커니즘을 시뮬레이션한다:

1. **CSI Migration 상태 머신**: In-tree 볼륨 플러그인에서 CSI 드라이버로의 점진적 전환 과정
2. **In-tree → CSI 볼륨 스펙 변환**: 기존 In-tree 볼륨 스펙을 CSI 형식으로 자동 변환
3. **Ephemeral Volume PVC 생성 라이프사이클**: Pod 수명주기에 바인딩된 PVC 자동 생성/삭제
4. **Owner Reference 기반 Garbage Collection**: Pod 삭제 시 종속 PVC 자동 정리
5. **Volume Lifecycle Mode 결정**: Persistent/Ephemeral 모드에 따른 마운트 파이프라인 분기

## 실행 방법

```bash
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다.

## 시뮬레이션 내용

### [1] CSI Migration 상태 머신

`PluginManager`를 통해 In-tree 플러그인의 마이그레이션 상태를 3단계(Alpha → Beta → GA)로 전환하며 각 단계별 함수 반환값 변화를 확인한다.

- `IsMigrationEnabledForPlugin()`: Beta 이상에서 true
- `IsMigrationCompleteForPlugin()`: GA에서만 true
- `IsMigratable()`: 지원 플러그인이고 Beta 이상이면 true

실제 소스: `pkg/volume/csimigration/plugin_manager.go`

### [2] In-tree → CSI 볼륨 스펙 변환

`TranslateInTreeSpecToCSI()`를 시뮬레이션하여 다양한 In-tree 볼륨 스펙(AWS EBS, GCE PD, Azure Disk)을 CSI 형식으로 변환한다. AWS EBS의 `aws://` 형식 볼륨 ID 정규화도 포함한다.

실제 소스: `pkg/volume/csimigration/plugin_manager.go` (라인 129-150)

### [3] Ephemeral Volume PVC 생성 라이프사이클

`EphemeralController`를 시뮬레이션하여 5가지 시나리오를 검증한다:
- 시나리오 1: Ephemeral 볼륨이 있는 Pod 생성 → PVC 자동 생성
- 시나리오 2: 동일 Pod 재처리 → 멱등성 확인 (중복 생성 방지)
- 시나리오 3: Ephemeral이 없는 Pod → 무시
- 시나리오 4: PVC 삭제 후 자가 치유 → PVC 재생성
- 시나리오 5: 삭제 중인 Pod → 무시

실제 소스: `pkg/controller/volume/ephemeral/controller.go`

### [4] Owner Reference 기반 Garbage Collection

Pod 삭제 시 `OwnerReference`를 기반으로 종속 PVC를 탐색하고 삭제하는 GC 동작을 시뮬레이션한다. `BlockOwnerDeletion=true`에 의한 foreground 삭제와, 다른 Owner(StatefulSet)의 PVC가 유지되는 것을 확인한다.

### [5] Volume Lifecycle Mode 결정

`getVolumeLifecycleMode()`를 시뮬레이션하여 볼륨 스펙 기반으로 Persistent/Ephemeral 모드를 결정하고, 각 모드에서의 `CanAttach()`, `CanDeviceMount()` 반환값 차이를 확인한다.

실제 소스: `pkg/volume/csi/csi_plugin.go` (라인 892-904)

## 대응 문서

- [34-storage-advanced.md](../34-storage-advanced.md)

## 참조 소스 코드

| 컴포넌트 | 소스 경로 |
|---------|-----------|
| PluginManager | `pkg/volume/csimigration/plugin_manager.go` |
| Ephemeral Controller | `pkg/controller/volume/ephemeral/controller.go` |
| CSIDriver 타입 | `staging/src/k8s.io/api/storage/v1/types.go` |
| CSI Plugin | `pkg/volume/csi/csi_plugin.go` |
| VolumeClaimName | `staging/src/k8s.io/component-helpers/storage/ephemeral/ephemeral.go` |
