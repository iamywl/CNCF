# PoC-30: Grafana Envelope Encryption (DEK/KEK) 시뮬레이션

## 개요

Grafana는 Envelope Encryption으로 데이터소스 비밀번호 등 민감한 데이터를 보호한다.
이 PoC는 DEK/KEK 패턴, AES-GCM 암복호화, KEK 로테이션을 시뮬레이션한다.

## 시뮬레이션하는 개념

| 개념 | 실제 Grafana 코드 | 시뮬레이션 |
|------|------------------|-----------|
| DEK | `pkg/services/encryption/` | 랜덤 256-bit 키 생성 |
| KEK | `pkg/services/kmsproviders/` | 마스터 키로 DEK 암호화 |
| AES-GCM | Go crypto/aes + cipher | 인증 암호화 |
| KEK Rotation | 키 로테이션 로직 | DEK 재암호화 |
| DEK Cache | 복호화 성능 최적화 | 메모리 캐시 |

## 실행 방법

```bash
cd grafana_EDU/poc-30-envelope-encryption
go run main.go
```

## 핵심 포인트

- **Envelope Encryption**: 데이터는 DEK로, DEK는 KEK로 이중 암호화
- **DEK 독립성**: 각 시크릿마다 새 DEK를 생성하여 한 DEK 노출 시 피해 최소화
- **KEK 로테이션**: 마스터 키 교체 시 데이터 재암호화 없이 DEK만 재암호화
