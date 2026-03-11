# PoC-26: Jenkins DependencyGraph (양방향 + Tarjan SCC) 시뮬레이션

## 개요

Jenkins DependencyGraph는 잡 간 의존 관계를 양방향으로 관리한다.
이 PoC는 양방향 그래프, Tarjan SCC(순환 감지), 위상 정렬을 구현한다.

## 실행 방법

```bash
cd jenkins_EDU/poc-26-dependency-graph
go run main.go
```
