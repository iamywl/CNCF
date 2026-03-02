# PoC: Hubble Getter 인터페이스 패턴

> **관련 문서**: [07-CODE-GUIDE.md](../07-CODE-GUIDE.md) - Getter 인터페이스 패턴, [04-SEQUENCE-DIAGRAMS.md](../04-SEQUENCE-DIAGRAMS.md) - Parser 디코딩 흐름

## 이 PoC가 보여주는 것

Hubble Parser의 **Getter 인터페이스를 통한 의존성 역전** 패턴을 보여줍니다.

```
Parser (소비자)
  │
  ├── EndpointGetter 인터페이스 ──→ K8sEndpointGetter (프로덕션)
  │                               └→ MockEndpointGetter (테스트)
  │
  ├── DNSGetter 인터페이스     ──→ K8sDNSGetter (프로덕션)
  │                               └→ MockDNSGetter (테스트)
  │
  ├── ServiceGetter 인터페이스 ──→ K8sServiceGetter
  └── IdentityGetter 인터페이스──→ K8sIdentityGetter
```

## 실행 방법

```bash
cd EDU/poc-getter-interface
go run main.go
```

## 관찰할 수 있는 것

1. **프로덕션 시나리오**: K8s 데이터로 IP → Pod/Service/DNS enrichment
2. **테스트 시나리오**: Mock으로 동일한 Parser 로직 검증
3. **핵심**: 같은 `Parser.Parse()` 코드가 다른 Getter 구현과 동작

## 핵심 학습 포인트

- **인터페이스 분리 원칙**: 각 메타데이터 소스가 별도 인터페이스
- **의존성 역전**: Parser가 구체 클래스가 아닌 인터페이스에 의존
- **테스트 용이성**: K8s 클러스터 없이 Parser 단위 테스트 가능
