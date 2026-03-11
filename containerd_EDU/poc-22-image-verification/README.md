# PoC-22: containerd 이미지 서명 검증 파이프라인 시뮬레이션

## 개요

containerd는 이미지 pull 시 서명을 검증하여 신뢰할 수 있는 이미지만 실행한다.
이 PoC는 ECDSA 서명, Trust Policy, 검증 파이프라인을 시뮬레이션한다.

## 시뮬레이션하는 개념

| 개념 | 실제 코드 | 시뮬레이션 |
|------|----------|-----------|
| Image Digest | OCI 매니페스트 SHA256 | 매니페스트 다이제스트 계산 |
| ECDSA Signing | Cosign/sigstore | P-256 키 생성 + 서명 |
| Trust Policy | Notation trust policy | 패턴별 검증 수준 설정 |
| Verification | 서명 조회 → 키 검증 | 파이프라인 실행 |

## 실행 방법

```bash
cd containerd_EDU/poc-22-image-verification
go run main.go
```
