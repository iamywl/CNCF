# PoC 26: DNS & Service Discovery 시뮬레이션

## 개요
Kubernetes의 DNS 기반 서비스 디스커버리 메커니즘을 시뮬레이션합니다.

## 다루는 개념
- **DNS Policy**: ClusterFirst, Default, None, ClusterFirstWithHostNet
- **Search Domain**: namespace.svc.cluster.local 순서
- **ndots:5**: 내부 Service 우선 해석
- **Service DNS**: A/AAAA, SRV, CNAME 레코드
- **Headless Service**: Pod별 A 레코드
- **환경변수 SD**: {SVC}_SERVICE_HOST/PORT

## 실행
```bash
go run main.go
```

## 참조 소스코드
| 기능 | 파일 |
|------|------|
| DNS Configurer | `pkg/kubelet/network/dns/dns.go` |
| 환경변수 SD | `pkg/kubelet/envvars/envvars.go` |
| Pod DNS | `pkg/kubelet/kubelet_pods.go` |
| DNS Policy 타입 | `pkg/apis/core/types.go` |
