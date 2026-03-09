# PoC-17: 리더 선출과 Status 관리 시뮬레이션

## 관련 문서
- [20-leader-status.md](../../istio_EDU/20-leader-status.md)

## 시뮬레이션 내용
1. **Kubernetes Lease 기반 리더 선출**: 다중 istiod 인스턴스 중 하나만 리더로 선출
2. **Status 배치 업데이트**: 중복 쓰기 방지를 통한 API 서버 부하 감소
3. **Distribution Reporting**: 설정이 모든 프록시에 ACK/NACK되었는지 추적
4. **리더 Failover**: 리더 다운 시 자동 인수 및 새 리더 선출
5. **NACK 처리**: 프록시가 설정 적용 실패 시 상태 반영

## 참조 소스
- `pilot/pkg/leaderelection/leaderelection.go`
- `pilot/pkg/status/state.go`
- `pilot/pkg/status/distribution.go`

## 실행
```bash
go run main.go
```
