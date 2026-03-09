# PoC-20: Ingress 호환 + WorkloadEntry 자동 등록 시뮬레이션

## 관련 문서
- [23-ingress-workloadentry.md](../../istio_EDU/23-ingress-workloadentry.md)

## 시뮬레이션 내용
1. **Ingress 필터링**: OFF/STRICT/DEFAULT 모드에 따른 Ingress 처리 여부 결정
2. **Gateway 변환**: Ingress TLS → HTTPS Server + 기본 HTTP Server
3. **VirtualService 변환**: 호스트별 규칙 그룹핑, Exact > Prefix 경로 정렬
4. **WorkloadEntry 자동 등록**: xDS 연결 시 WorkloadGroup 템플릿으로 WLE 생성
5. **다중 연결 추적**: 모든 연결 해제 시에만 정리, 유예 기간 내 재접속 시 삭제 취소

## 참조 소스
- `pilot/pkg/config/kube/ingress/controller.go`
- `pilot/pkg/config/kube/ingress/virtualservices.go`
- `pilot/pkg/config/kube/ingress/gateways.go`
- `pilot/pkg/autoregistration/controller.go`
- `pilot/pkg/autoregistration/connections.go`

## 실행
```bash
go run main.go
```
