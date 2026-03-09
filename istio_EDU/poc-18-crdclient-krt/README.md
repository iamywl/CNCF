# PoC-18: CRD Client + krt 프레임워크 시뮬레이션

## 관련 문서
- [21-crdclient-krt.md](../../istio_EDU/21-crdclient-krt.md)

## 시뮬레이션 내용
1. **ConfigStore**: Kubernetes CRD의 CRUD + Watch 이벤트 시스템
2. **krt.Collection**: 선언적 데이터 변환 파이프라인 (Config → RouteEntry)
3. **krt.Index**: 특정 키로 O(1) 인덱스 조회
4. **변경 전파**: 입력 변경 시 파생 컬렉션 자동 갱신

## 참조 소스
- `pilot/pkg/config/kube/crdclient/client.go`
- `pkg/kube/krt/collection.go`
- `pkg/kube/krt/index.go`

## 실행
```bash
go run main.go
```
