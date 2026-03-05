# PoC-16: PV/PVC 바인딩 및 볼륨 수명주기 시뮬레이션

## 개요

Kubernetes PersistentVolume/PersistentVolumeClaim의 바인딩 알고리즘, 동적 프로비저닝, Reclaim 정책을 시뮬레이션한다.

## 시뮬레이션 대상

| 컴포넌트 | 실제 소스 | 시뮬레이션 |
|----------|----------|-----------|
| PV/PVC 모델 | `pkg/apis/core/types.go` | 용량, AccessMode, ReclaimPolicy, StorageClassName |
| PV Controller | `pkg/controller/volume/persistentvolume/pv_controller.go` | syncVolume, syncClaim, bind |
| 매칭 알고리즘 | `pkg/controller/volume/persistentvolume/index.go` | findBestMatchForClaim, AccessMode 인덱싱 |
| 동적 프로비저닝 | CSI 외부 provisioner | StorageClass → PV 자동 생성 |

## 핵심 알고리즘

```
PV 매칭 기준 (순서대로):
  1. AccessModes: PV 모드 ⊇ PVC 요구 모드
  2. StorageClass: 동일한 클래스
  3. 용량: PV 용량 ≥ PVC 요청
  4. 레이블 셀렉터: PVC.Selector ⊆ PV.Labels
  5. 최소 낭비: 만족하는 PV 중 최소 용량

수명주기:
  Available → Bound → Released → Retain/Delete/Recycle
```

## 실행

```bash
go run main.go
```

## 데모 항목

1. **정적 프로비저닝**: 다양한 PV 중 최적 매칭 (용량, AccessMode, StorageClass)
2. **레이블 셀렉터**: zone 레이블로 특정 PV 선택
3. **동적 프로비저닝**: StorageClass → CSI provisioner → PV 자동 생성
4. **Reclaim 정책**: Retain(보존)/Delete(삭제)/Recycle(재활용) 비교
5. **수명주기 추적**: Available → Bound → Released 전이
