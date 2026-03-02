# kube-health-checker

> Kubernetes 클러스터의 헬스 상태를 모니터링하는 CLI 도구

![Build](https://img.shields.io/badge/build-passing-brightgreen)
![Go Version](https://img.shields.io/badge/go-1.21-blue)
![License](https://img.shields.io/badge/license-Apache%202.0-green)
![Coverage](https://img.shields.io/badge/coverage-87%25-yellowgreen)

## 왜 만들었나?

`kubectl get nodes`와 `kubectl top`을 매번 따로 실행하는 것이 번거로워서,
**한 번의 명령으로** 클러스터 전체 상태를 요약하는 도구를 만들었습니다.

```
$ kube-health

Cluster: my-production (v1.28.3)
┌──────────────┬────────┬─────────┬─────────┬───────────┐
│ Node         │ Status │ CPU (%) │ Mem (%) │ Pods      │
├──────────────┼────────┼─────────┼─────────┼───────────┤
│ node-01      │ Ready  │ 45%     │ 62%     │ 28/110    │
│ node-02      │ Ready  │ 38%     │ 55%     │ 32/110    │
│ node-03      │ Ready  │ 72%     │ 81%     │ 45/110    │ ⚠️ High
└──────────────┴────────┴─────────┴─────────┴───────────┘

Warnings:
  ⚠️  node-03: Memory usage above 80%
  ⚠️  3 pods in CrashLoopBackOff (namespace: default)
```

## 주요 기능

- **노드 상태 요약**: CPU, 메모리, Pod 수를 한눈에
- **이상 감지**: 높은 리소스 사용, CrashLoopBackOff, Pending Pod 자동 경고
- **멀티 클러스터**: kubeconfig의 모든 컨텍스트를 순회
- **출력 형식**: 테이블, JSON, YAML 지원

## 설치

### Homebrew (macOS/Linux)
```bash
brew install username/tap/kube-health
```

### Go Install
```bash
go install github.com/username/kube-health@latest
```

### Binary 직접 다운로드
[Releases](https://github.com/username/kube-health/releases) 페이지에서 OS/아키텍처에 맞는 바이너리를 다운로드하세요.

## 사용법

```bash
# 기본 사용 (현재 컨텍스트)
kube-health

# 특정 컨텍스트
kube-health --context production

# JSON 출력 (스크립트 연동용)
kube-health --output json

# 특정 네임스페이스 Pod만
kube-health --namespace kube-system

# 모든 컨텍스트 순회
kube-health --all-contexts
```

<details>
<summary>고급 사용법</summary>

### 알림 연동
```bash
# Slack 웹훅으로 경고 전송
kube-health --alert-webhook https://hooks.slack.com/...

# 임계값 커스텀
kube-health --cpu-threshold 90 --memory-threshold 85
```

### 크론잡으로 주기적 체크
```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: cluster-health-check
spec:
  schedule: "*/30 * * * *"
  jobTemplate:
    spec:
      template:
        spec:
          containers:
          - name: health-checker
            image: username/kube-health:latest
            args: ["--output", "json", "--alert-webhook", "$(SLACK_WEBHOOK)"]
```

</details>

## 설정 파일

`~/.kube-health.yaml`:
```yaml
defaults:
  output: table
  cpu_threshold: 80
  memory_threshold: 80

alerts:
  slack_webhook: https://hooks.slack.com/...

ignore:
  namespaces:
    - kube-system
    - monitoring
```

## 기여

기여를 환영합니다! [CONTRIBUTING.md](CONTRIBUTING.md)를 참고해 주세요.

## 라이선스

[Apache License 2.0](LICENSE)
