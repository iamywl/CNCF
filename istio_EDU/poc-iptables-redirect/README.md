# PoC: Istio iptables 트래픽 인터셉션 규칙 생성

## 개요

Istio가 사이드카(Envoy) 또는 ztunnel로 트래픽을 투명하게 리다이렉트하기 위해 생성하는
iptables 규칙의 구조와 로직을 시뮬레이션한다.

## 실제 소스 코드 참조

| 파일 | 역할 |
|------|------|
| `tools/istio-iptables/pkg/builder/iptables_builder_impl.go` | Rule 구조체, IptablesRuleBuilder 빌더 패턴 |
| `tools/istio-iptables/pkg/capture/run.go` | 사이드카 모드 규칙 생성 (handleInboundPortsInclude, Run) |
| `tools/istio-iptables/pkg/constants/constants.go` | 체인 이름, 포트 상수, 빌트인 체인 목록 |
| `cni/pkg/iptables/iptables.go` | Ambient 모드 인팟 규칙 생성 (AppendInpodRules) |

## 핵심 개념

### 체인 모델 (사이드카 모드)

```
PREROUTING ──→ ISTIO_INBOUND ──→ ISTIO_IN_REDIRECT ──→ Envoy(:15006)
                    │
                    └── port 15008,15090,15021 → RETURN (제외)

OUTPUT ──→ ISTIO_OUTPUT ──→ ISTIO_REDIRECT ──→ Envoy(:15001)
                │
                ├── UID 1337 → RETURN (무한 루프 방지)
                ├── dst 127.0.0.1 → RETURN (로컬 스킵)
                └── DNS(port 53) → REDIRECT(:15053)
```

### 무한 루프 방지 (UID 1337)

Envoy 프로세스는 UID 1337로 실행된다. `ISTIO_OUTPUT` 체인에서 UID 1337의 트래픽을
RETURN하여 다시 Envoy로 리다이렉트되는 무한 루프를 방지한다.

### Ambient 모드 차이점

- UID 대신 패킷 마크(0x539)로 ztunnel 트래픽을 식별
- mangle/raw 테이블을 추가로 사용하여 connmark 기반 추적
- HBONE 터널(15008)은 직접 통과

## 시뮬레이션 내용

1. 사이드카 모드 iptables-restore 형식 규칙 생성
2. 인바운드/아웃바운드 트래픽 흐름 시각화
3. 포트 제외 로직 검증
4. Ambient 모드 인팟 규칙 생성
5. 사이드카 vs Ambient 모드 비교
6. DNS 캡처 메커니즘 상세

## 실행

```bash
go run main.go
```
