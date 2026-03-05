# PoC-02: 쿠버네티스 데이터 모델 패턴

## 개요

쿠버네티스의 모든 API 리소스가 따르는 **TypeMeta + ObjectMeta + Spec + Status** 패턴을 구현한다.
추가로 OwnerReference를 이용한 가비지 컬렉션, ResourceVersion을 이용한 낙관적 동시성 제어,
라벨 셀렉터 매칭을 시뮬레이션한다.

## 구현 내용

| 개념 | 설명 | 실제 소스 위치 |
|------|------|-------------|
| TypeMeta | Kind + APIVersion (리소스 타입 식별) | `apimachinery/pkg/apis/meta/v1/types.go:42` |
| ObjectMeta | Name, UID, Labels, OwnerRef 등 | `apimachinery/pkg/apis/meta/v1/types.go:111` |
| Spec/Status | 원하는 상태 vs 현재 상태 분리 | `api/core/v1/types.go` |
| OwnerReference | 소유 관계 → 계단식 GC | `pkg/controller/garbagecollector/` |
| ResourceVersion | 낙관적 동시성 제어 | `apimachinery/pkg/apis/meta/v1/types.go:172` |
| LabelSelector | AND 매칭으로 리소스 연결 | `apimachinery/pkg/labels/selector.go` |

## 시뮬레이션 시나리오

1. Deployment → ReplicaSet → Pod 계층 구조 생성
2. 라벨 셀렉터를 이용한 파드 필터링 및 Service 연결
3. 낙관적 동시성 제어 (정상 업데이트 + 충돌 감지)
4. Deployment 삭제 시 GC를 통한 계단식 정리
5. K8s API 호환 JSON 직렬화

## 실행

```bash
go run main.go
```
