# PoC 06: Cilium 설정 체계 및 디버깅 체험

Cilium의 다중 소스 설정 체계(플래그 > 환경변수 > 파일 > 기본값)와
트러블슈팅 과정을 시뮬레이션한다.

---

## 핵심 매커니즘

```
설정 우선순위 (높은 것이 덮어씀):

1. 커맨드라인 플래그       --enable-policy=always
2. 환경 변수              CILIUM_ENABLE_POLICY=always
3. 설정 파일              ciliumd.yaml: enable-policy: always
4. ConfigDir              /etc/cilium/enable-policy
5. 기본값                 "default"

실제 코드: pkg/option/config.go → DaemonConfig 구조체
```

## 실행 방법

```bash
cd EDU/poc-06-operations

# 기본 실행 — 기본값으로 동작
go run main.go

# 플래그로 오버라이드
go run main.go --enable-policy=always --routing-mode=native

# 환경 변수로 오버라이드
CILIUM_ENABLE_HUBBLE=true go run main.go

# 설정 파일 사용
go run main.go --config=sample-config.yaml

# 트러블슈팅 시뮬레이션
go run main.go --simulate-trouble
```
