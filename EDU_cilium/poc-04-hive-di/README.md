# PoC 04: Hive 의존성 주입 프레임워크 체험

Cilium이 사용하는 Hive DI 프레임워크의 동작 원리를 직접 체험한다.
컴포넌트 등록, 생명주기 관리, 의존성 자동 해결을 구현해본다.

---

## 핵심 매커니즘

```
Hive가 하는 일:

1. Cell 등록: "나는 PolicyEngine이고, KVStore와 EndpointManager가 필요해"
2. 의존성 해결: KVStore 먼저 생성 → EndpointManager 생성 → PolicyEngine 생성
3. 생명주기 관리: Start()를 의존성 순서대로 호출, Stop()은 역순으로 호출

┌── Hive ──────────────────────────────────┐
│                                          │
│  KVStore ──► EndpointManager             │
│      │              │                    │
│      └──────┬───────┘                    │
│             ▼                            │
│       PolicyEngine ──► BPF Loader        │
│                                          │
│  Start 순서: KVStore → EndpointMgr →     │
│              PolicyEngine → BPFLoader    │
│  Stop 순서:  BPFLoader → PolicyEngine →  │
│              EndpointMgr → KVStore       │
└──────────────────────────────────────────┘
```

Cilium에 수십 개의 컴포넌트가 있으므로 수동으로 순서를 관리하면 실수가 발생한다.
Hive가 의존성 그래프를 분석하여 자동으로 올바른 순서를 결정한다.

## 실행 방법

```bash
cd EDU/poc-04-hive-di
go run main.go
```
