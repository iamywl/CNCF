# PoC-25: Jenkins Cron Scheduler (H Hash + Bitmask) 시뮬레이션

## 개요

Jenkins cron은 표준 cron에 H 해시 기능을 추가하여 잡 실행을 분산시킨다.
이 PoC는 H 토큰 해싱, 비트마스크 CronTab, 매칭을 시뮬레이션한다.

## 실행 방법

```bash
cd jenkins_EDU/poc-25-cron-scheduler
go run main.go
```
