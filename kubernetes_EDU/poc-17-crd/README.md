# PoC-17: CustomResourceDefinition (CRD) 처리 시뮬레이션

## 개요

Kubernetes CRD의 등록, 동적 REST 핸들러 생성, 스키마 검증, 버전 관리를 시뮬레이션한다.

## 시뮬레이션 대상

| 컴포넌트 | 실제 소스 | 시뮬레이션 |
|----------|----------|-----------|
| CRD 타입 | `staging/.../apiextensions/v1/types.go` | Group, Names, Scope, Versions 정의 |
| 동적 핸들러 | `staging/.../apiserver/customresource_handler.go` | CRD 등록 시 REST 저장소 동적 생성 |
| REST 저장소 | `staging/.../registry/customresource/etcd.go` | CRUD 오퍼레이션 (인메모리 맵) |
| 스키마 검증 | `staging/.../apiserver/schema/validation.go` | Required, Type, Min/Max, Enum 검증 |
| 버전 관리 | CRD Versions[] | Served/Storage 구분, 버전별 스키마 |

## 핵심 동작

```
CRD 등록 흐름:
  1. CRD 생성: { Group, Names, Versions[{Name, Schema}], Scope }
  2. apiextensions-apiserver 감지 → REST 핸들러 동적 등록
  3. API 경로 생성:
     Namespaced: /apis/<group>/<ver>/namespaces/<ns>/<plural>
     Cluster:    /apis/<group>/<ver>/<plural>
  4. CRUD 요청마다 버전별 스키마 검증

스키마 검증:
  - Required: 필수 필드 존재 확인
  - Type: string/integer/boolean/object/array
  - Minimum/Maximum: 숫자 범위 제한
  - Enum: 허용 값 목록
  - Unknown fields: 미정의 필드 거부
```

## 실행

```bash
go run main.go
```

## 데모 항목

1. **CRD 등록**: CronTab CRD (3개 버전: v1alpha1, v1beta1, v1)
2. **CRUD**: Create, Read, Update, List, Delete 전체 오퍼레이션
3. **스키마 검증**: 필수 필드, 타입, 범위, Enum, 미정의 필드 거부
4. **버전별 스키마**: v1alpha1(max=10) vs v1beta1(max=100) vs v1(max=1000)
5. **Cluster-Scoped CRD**: namespace 없는 클러스터 범위 리소스
