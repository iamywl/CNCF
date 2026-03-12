# PoC-07: 서비스 디스커버리

CoreDNS kubernetes 플러그인(`plugin/kubernetes/`)의 서비스 → DNS 매핑을 시뮬레이션하는 PoC.

## 재현하는 CoreDNS 내부 구조

| 구성 요소 | 실제 소스 위치 | PoC 재현 내용 |
|-----------|---------------|--------------|
| parseRequest() | `plugin/kubernetes/parse.go` | DNS 이름 → K8s 쿼리 요소 파싱 |
| ServeDNS() | `plugin/kubernetes/handler.go` | 쿼리 타입별 분기 처리 |
| findServices() | `plugin/kubernetes/kubernetes.go` | 서비스 조회 + A/SRV 레코드 생성 |
| serviceRecordForIP() | `plugin/kubernetes/reverse.go` | IP → 서비스 역방향 조회 |
| endpointHostname() | `plugin/kubernetes/kubernetes.go` | 엔드포인트 호스트명 생성 |

## Kubernetes DNS 스키마 (v1.1.0)

### A 레코드
```
<service>.<namespace>.svc.cluster.local → ClusterIP
```

### SRV 레코드
```
_<port>._<protocol>.<service>.<namespace>.svc.cluster.local → port + target
```

### PTR 레코드 (역방향)
```
<reversed-ip>.in-addr.arpa → <service>.<namespace>.svc.cluster.local
```

### 서비스 타입별 동작
| 타입 | A 쿼리 결과 | SRV 쿼리 결과 |
|------|-----------|-------------|
| ClusterIP | ClusterIP 주소 | 포트별 SRV 레코드 |
| Headless | Pod IP 목록 | 엔드포인트별 SRV |
| ExternalName | CNAME (외부 도메인) | 미지원 |

## 실행 방법

```bash
go run main.go
```

## 데모 내용

1. **A 레코드 쿼리**: ClusterIP 서비스의 IP 반환
2. **Headless 서비스**: Pod IP 직접 반환
3. **SRV 레코드**: 포트 정보 포함 응답
4. **ExternalName**: CNAME으로 외부 서비스 연결
5. **PTR 역방향 조회**: ClusterIP → 서비스 FQDN
6. **DNS 이름 파싱**: parseRequest() 알고리즘
7. **NXDOMAIN 처리**: 존재하지 않는 서비스/네임스페이스
8. **엔드포인트 호스트명**: IP → hostname 변환 규칙
