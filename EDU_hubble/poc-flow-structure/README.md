# PoC: Hubble Flow 데이터 구조

> **관련 문서**: [03-DATA-MODEL.md](../03-DATA-MODEL.md) - 핵심 데이터 엔티티 관계도, Flow, 프로토콜 계층별 데이터

## 이 PoC가 보여주는 것

Hubble의 Flow 구조체가 네트워크 패킷을 **계층적으로 분리**하여 표현하는 방식을 보여줍니다.

```
Flow
├── Endpoint (source)      ← K8s 메타데이터 (Pod, Namespace, Labels)
├── Endpoint (destination)
├── Ethernet (L2)          ← MAC 주소
├── IP (L3)                ← IP 주소, 버전, 암호화
├── L4 (oneof)             ← TCP | UDP | ICMP (상호 배타)
├── L7 (oneof)             ← DNS | HTTP (상호 배타)
├── Verdict                ← FORWARDED | DROPPED | REDIRECTED
└── Summary                ← 사람이 읽을 수 있는 요약
```

## 실행 방법

```bash
cd EDU/poc-flow-structure
go run main.go
```

## 4가지 시나리오

1. **TCP SYN**: frontend → backend 연결 시도
2. **DNS 조회**: frontend가 backend 서비스 이름 해석
3. **정책 차단**: untrusted Pod이 database 접근 시도 → DROPPED
4. **HTTP 요청**: frontend → backend API GET /api/v1/users

## 핵심 학습 포인트

- **oneof 패턴**: L4는 TCP/UDP/ICMP 중 하나만, L7은 DNS/HTTP 중 하나만
- **Identity 기반**: IP보다 `namespace/pod-name (labels)`가 K8s 환경에서 유용
- **Verdict**: 모든 Flow에 정책 판정 결과가 포함되어 "왜 차단되었는가"를 바로 파악
