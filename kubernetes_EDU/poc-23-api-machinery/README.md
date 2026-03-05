# PoC 23: API Machinery 시뮬레이션

## 개요
Kubernetes API Machinery의 핵심 메커니즘(Scheme, Conversion, SMP, SSA)을 시뮬레이션합니다.

## 다루는 개념
- **Scheme**: GVK ↔ Go Type 양방향 매핑, 타입 레지스트리
- **Hub-and-Spoke 변환**: internal type을 허브로 N개 버전을 2N개 변환으로 처리
- **Strategic Merge Patch**: 중첩 map 재귀 병합, patch directives
- **Server-Side Apply**: FieldManager, 필드 소유권 추적, 충돌 감지
- **Defaulting**: 미지정 필드에 기본값 자동 적용

## 실행
```bash
go run main.go
```

## 참조 소스코드
| 기능 | 파일 |
|------|------|
| Scheme | `staging/src/k8s.io/apimachinery/pkg/runtime/scheme.go` |
| runtime.Object | `staging/src/k8s.io/apimachinery/pkg/runtime/interfaces.go` |
| SMP | `staging/src/k8s.io/apimachinery/pkg/util/strategicpatch/patch.go` |
| FieldManager | `staging/src/k8s.io/apimachinery/pkg/util/managedfields/fieldmanager.go` |
