# PoC: Hubble 설정 우선순위 시스템

> **관련 문서**: [06-OPERATIONS.md](../06-OPERATIONS.md) - 설정, [04-SEQUENCE-DIAGRAMS.md](../04-SEQUENCE-DIAGRAMS.md) - 설정 로드 시퀀스

## 이 PoC가 보여주는 것

Hubble의 **4단계 설정 우선순위**를 직접 실험할 수 있습니다.

```
우선순위 (높은 순):
  1. CLI 플래그      --server relay:4245
  2. 환경 변수       HUBBLE_SERVER=relay:4245
  3. 설정 파일       config.yaml → server: relay:4245
  4. 기본값          localhost:4245
```

## 실행 방법

```bash
cd EDU/poc-config-priority

# 1. 기본값만 사용 (설정 파일이 자동 생성됨)
go run main.go

# 2. 환경 변수로 오버라이드
MINI_HUBBLE_SERVER=env-relay:4245 go run main.go

# 3. CLI 플래그가 환경 변수보다 우선
MINI_HUBBLE_SERVER=env-relay:4245 go run main.go --server=flag-relay:4245

# 4. 여러 설정 동시에
MINI_HUBBLE_TLS=true go run main.go --timeout=30s --debug=true
```

## 실험 포인트

1. 아무 인자 없이 실행 → 기본값과 설정 파일 값 확인
2. 환경 변수 설정 후 실행 → 설정 파일 값이 오버라이드되는 것 확인
3. CLI 플래그 추가 → 환경 변수가 오버라이드되는 것 확인
4. 각 설정값 옆에 **어디서 왔는지(source)** 표시됨

## 핵심 학습 포인트

- **Viper 패턴**: 여러 소스를 자동으로 통합 관리
- **HUBBLE_ 접두어**: `--server` → `HUBBLE_SERVER` 자동 매핑
- **대시→밑줄 변환**: `--tls-allow-insecure` → `HUBBLE_TLS_ALLOW_INSECURE`
