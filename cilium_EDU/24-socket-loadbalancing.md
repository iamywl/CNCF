# 24. Socket 기반 로드밸런싱 (Socket-Level Load Balancing)

> Cilium 소스 기준: `pkg/socketlb/socketlb.go`, `pkg/socketlb/cgroup.go`

---

## 1. 개요

### 1.1 Socket LB란?

Socket LB는 **cgroup BPF** 프로그램을 사용하여 소켓 시스템 콜 레벨에서 로드밸런싱을 수행하는 Cilium 기능이다. 기존 iptables/DNAT 기반 kube-proxy를 완전히 대체한다.

```
kube-proxy (iptables) 방식:
  App → connect() → TCP SYN → iptables DNAT → Backend
  (패킷이 네트워크 스택을 통과한 후 DNAT)

Socket LB 방식:
  App → connect() → BPF 훅 → 주소 변환 → TCP SYN → Backend
  (시스템 콜 레벨에서 주소 변환, 패킷 생성 전)
```

### 1.2 왜 Socket 레벨에서 LB하는가?

| 비교 항목 | iptables DNAT | Socket LB |
|-----------|--------------|-----------|
| 처리 시점 | 패킷 전송 후 | 소켓 연결 전 |
| ConnTrack 필요 | 필수 (DNAT 역변환) | 불필요 |
| 레이턴시 | 높음 (netfilter 통과) | 매우 낮음 |
| CPU 사용 | 높음 | 낮음 |
| 소스 IP | NAT으로 변경됨 | 원본 유지 가능 |
| localhost 통신 | 복잡한 헤어핀 NAT | 자연스럽게 동작 |

### 1.3 아키텍처 개요

```
┌──────────────────────────────────────────────────────────┐
│                   Socket LB 아키텍처                      │
│                                                            │
│  ┌──────────┐    cgroup BPF 훅      ┌─────────────────┐  │
│  │   App    │ ──── connect() ────── │ cil_sock4_connect│  │
│  │ (socket) │                       │ (BPF 프로그램)    │  │
│  └──────────┘                       └────────┬────────┘  │
│                                              │            │
│                                     서비스 IP → 백엔드 IP  │
│                                     변환 수행              │
│                                              │            │
│  추가 cgroup BPF 프로그램:                     │            │
│  ┌──────────────────────────────────────────┐ │            │
│  │ cil_sock4_sendmsg   - UDP 송신 시 변환    │ │            │
│  │ cil_sock4_recvmsg   - UDP 수신 시 역변환  │ │            │
│  │ cil_sock4_getpeername - 피어 이름 조회     │ │            │
│  │ cil_sock4_post_bind  - 바인드 보호        │ │            │
│  │ cil_sock4_pre_bind   - 헬스체크 바인드    │ │            │
│  │ cil_sock_release     - 소켓 해제 정리     │ │            │
│  └──────────────────────────────────────────┘ │            │
│                                              ▼            │
│                                    ┌─────────────────┐    │
│                                    │   Backend Pod   │    │
│                                    └─────────────────┘    │
└──────────────────────────────────────────────────────────┘
```

---

## 2. cgroup BPF 프로그램 목록

### 2.1 프로그램 정의 (`pkg/socketlb/socketlb.go:27`)

```go
const (
    Connect4     = "cil_sock4_connect"     // IPv4 TCP/UDP connect()
    SendMsg4     = "cil_sock4_sendmsg"     // IPv4 UDP sendmsg()
    RecvMsg4     = "cil_sock4_recvmsg"     // IPv4 UDP recvmsg()
    GetPeerName4 = "cil_sock4_getpeername" // IPv4 getpeername()
    PostBind4    = "cil_sock4_post_bind"   // IPv4 바인드 후
    PreBind4     = "cil_sock4_pre_bind"    // IPv4 바인드 전
    Connect6     = "cil_sock6_connect"     // IPv6 TCP/UDP connect()
    SendMsg6     = "cil_sock6_sendmsg"     // IPv6 UDP sendmsg()
    RecvMsg6     = "cil_sock6_recvmsg"     // IPv6 UDP recvmsg()
    GetPeerName6 = "cil_sock6_getpeername" // IPv6 getpeername()
    PostBind6    = "cil_sock6_post_bind"   // IPv6 바인드 후
    PreBind6     = "cil_sock6_pre_bind"    // IPv6 바인드 전
    SockRelease  = "cil_sock_release"      // 소켓 해제
)
```

### 2.2 각 프로그램의 역할

```
┌────────────────────────────────────────────────────────────┐
│ TCP 흐름:                                                   │
│                                                              │
│  connect() → cil_sock4_connect                              │
│    서비스 VIP:Port → 백엔드 IP:Port로 변환                    │
│    (커널이 변환된 주소로 SYN 전송)                             │
│                                                              │
│  getpeername() → cil_sock4_getpeername                       │
│    백엔드 IP:Port → 서비스 VIP:Port로 역변환                  │
│    (앱이 원래 연결한 주소를 볼 수 있도록)                       │
│                                                              │
│  close() → cil_sock_release                                 │
│    LB 상태 정리                                              │
├────────────────────────────────────────────────────────────┤
│ UDP 흐름:                                                   │
│                                                              │
│  sendmsg() → cil_sock4_sendmsg                              │
│    매 패킷마다 서비스 VIP → 백엔드 IP 변환                    │
│    (UDP는 연결 상태가 없으므로 매번 변환)                       │
│                                                              │
│  recvmsg() → cil_sock4_recvmsg                              │
│    응답 패킷의 소스를 서비스 VIP로 역변환                      │
│    (앱이 원래 서비스 주소에서 응답받는 것처럼 보이게)            │
├────────────────────────────────────────────────────────────┤
│ 바인드 보호:                                                 │
│                                                              │
│  bind() → cil_sock4_post_bind                               │
│    NodePort 포트 범위를 앱이 바인드하지 못하게 보호             │
│                                                              │
│  bind() → cil_sock4_pre_bind                                │
│    헬스 데이터패스를 위한 사전 바인드 처리                      │
└────────────────────────────────────────────────────────────┘
```

---

## 3. 소스 코드 분석

### 3.1 Enable 함수 (`pkg/socketlb/socketlb.go:68`)

```go
func Enable(logger *slog.Logger, reg *registry.MapRegistry,
    sysctl sysctl.Sysctl, lnc *datapath.LocalNodeConfiguration) error {

    // 1. bpffs 링크 디렉토리 생성
    os.MkdirAll(cgroupLinkPath(), 0777)

    // 2. 컴파일된 BPF 오브젝트 로드
    spec, err := ebpf.LoadCollectionSpec(
        filepath.Join(option.Config.StateDir, socketObj))

    // 3. BPF 설정 구성
    cfg := config.NewBPFSock(config.NodeConfig(lnc))

    // 4. BPF 컬렉션 로드 (맵 공유, 상수 주입)
    coll, commit, err := bpf.LoadCollection(logger, spec, &bpf.CollectionOptions{
        MapRegistry: reg,
        Constants:   cfg,
    })

    // 5. 프로그램별 활성화/비활성화 결정
    enabled := make(map[string]bool)
    if option.Config.EnableIPv4 {
        enabled[Connect4] = true
        enabled[SendMsg4] = true
        enabled[RecvMsg4] = true
        // ...조건부 프로그램들
    }

    // 6. 활성화된 프로그램을 cgroup에 부착
    for p, s := range enabled {
        if s {
            attachCgroup(logger, coll, p, cgroups.GetCgroupRoot(), cgroupLinkPath())
        } else {
            detachCgroup(logger, p, cgroups.GetCgroupRoot(), cgroupLinkPath())
        }
    }

    // 7. BPF 핀 커밋
    commit()
}
```

### 3.2 cgroup 부착 메커니즘 (`pkg/socketlb/cgroup.go`)

두 가지 커널 API를 지원한다:

```
┌─────────────────────────────────────────────────────┐
│ 커널 >= 5.7: bpf_link API                            │
│                                                       │
│  1. 기존 핀된 link 열기 시도 (UpdateLink)              │
│     → 성공: 프로그램 원자적 교체 완료                   │
│                                                       │
│  2. 새 link 생성 (AttachRawLink)                      │
│     → 성공: bpffs에 핀 → 프로세스 종료 후에도 유지    │
│                                                       │
│  장점: 원자적 교체, 프로세스 독립적                     │
├─────────────────────────────────────────────────────┤
│ 커널 < 5.7: PROG_ATTACH API                          │
│                                                       │
│  RawAttachProgram()으로 cgroup에 직접 부착             │
│                                                       │
│  장점: 호환성, cgroup이 참조 유지                      │
│  단점: 원자적 교체 보장 어려움                         │
└─────────────────────────────────────────────────────┘
```

**왜 두 API를 모두 지원하는가?**

업그레이드 호환성. 이전 Cilium 버전은 PROG_ATTACH를 사용했으므로, 업그레이드 시 기존 프로그램을 PROG_ATTACH로 교체한 후 bpf_link로 전환한다. 연결 중단 없이 점진적 마이그레이션이 가능하다.

### 3.3 AttachType 매핑

```go
// pkg/socketlb/cgroup.go:38
var attachTypes = map[string]ebpf.AttachType{
    Connect4:     ebpf.AttachCGroupInet4Connect,
    SendMsg4:     ebpf.AttachCGroupUDP4Sendmsg,
    RecvMsg4:     ebpf.AttachCGroupUDP4Recvmsg,
    GetPeerName4: ebpf.AttachCgroupInet4GetPeername,
    PostBind4:    ebpf.AttachCGroupInet4PostBind,
    PreBind4:     ebpf.AttachCGroupInet4Bind,
    // ... IPv6 동일
    SockRelease:  ebpf.AttachCgroupInetSockRelease,
}
```

---

## 4. 동작 원리 상세

### 4.1 TCP connect() 변환

```
앱: connect(fd, {서비스IP=10.96.0.1, Port=80})

BPF 훅 (cil_sock4_connect):
  1. 서비스 맵 조회: 10.96.0.1:80 → Service{id=1}
  2. 백엔드 선택: Maglev 해시 → Backend{10.244.1.5:8080}
  3. 소켓 주소 변환:
     dst_addr = 10.244.1.5
     dst_port = 8080
  4. LB 상태 저장 (sock_ops 맵)

커널: connect(fd, {10.244.1.5:8080})
  → SYN 패킷이 백엔드로 직접 전송
  → iptables DNAT 불필요
  → ConnTrack 엔트리 불필요
```

### 4.2 UDP sendmsg() 변환

```
앱: sendmsg(fd, buf, {서비스IP=10.96.0.10, Port=53})

BPF 훅 (cil_sock4_sendmsg):
  1. 서비스 맵 조회: 10.96.0.10:53 → kube-dns Service
  2. 백엔드 선택: 10.244.2.100:53
  3. 패킷 주소 변환

수신 시 (cil_sock4_recvmsg):
  1. 응답의 소스 주소를 서비스 VIP로 역변환
  2. 앱은 10.96.0.10:53에서 응답받은 것으로 인식
```

### 4.3 getpeername() 역변환

```
앱이 getpeername()를 호출하면:
  실제 연결: 10.244.1.5:8080
  BPF 변환: 10.96.0.1:80 (원래 서비스 VIP 반환)

왜 필요한가:
  - 앱이 서비스 디스커버리 목적으로 getpeername() 사용
  - 로그에 서비스 VIP가 표시되어야 디버깅 가능
  - Envoy 같은 프록시가 원래 목적지를 알아야 함
```

### 4.4 NodePort 바인드 보호

```go
// PostBind4가 활성화되는 조건
if lnc.KPRConfig.KubeProxyReplacement && option.Config.NodePortBindProtection {
    enabled[PostBind4] = true
}
```

NodePort 범위(30000-32767)의 포트를 일반 앱이 bind()하면 충돌이 발생한다. `cil_sock4_post_bind`가 이 범위의 바인드를 차단하여 보호한다.

---

## 5. BPF 맵 공유

### 5.1 서비스/백엔드 맵

Socket LB BPF 프로그램은 기존 Cilium 서비스 맵을 공유한다:

```
공유 맵 구조:
┌────────────────────┐
│ cilium_lb4_services│ ← Socket LB + TC 공유
│ key: SvcKey4       │    (서비스 VIP:Port → 서비스 ID)
│ val: SvcValue4     │
├────────────────────┤
│ cilium_lb4_backends│ ← Socket LB + TC 공유
│ key: BackendID     │    (백엔드 ID → IP:Port)
│ val: BackendValue4 │
├────────────────────┤
│ cilium_sock_ops    │ ← Socket LB 전용
│ key: SockOpsKey    │    (소켓별 LB 상태)
│ val: SockOpsValue  │
└────────────────────┘
```

`MapRegistry`를 통해 TC 프로그램과 Socket LB 프로그램이 동일한 핀된 맵을 공유한다.

---

## 6. 조건부 프로그램 활성화

```go
// pkg/socketlb/socketlb.go:111-151
if option.Config.EnableIPv4 {
    enabled[Connect4] = true     // 항상 (TCP LB)
    enabled[SendMsg4] = true     // 항상 (UDP 송신 LB)
    enabled[RecvMsg4] = true     // 항상 (UDP 수신 역변환)

    if option.Config.EnableSocketLBPeer {
        enabled[GetPeerName4] = true   // 선택 (피어 이름 역변환)
    }
    if lnc.KPRConfig.KubeProxyReplacement && option.Config.NodePortBindProtection {
        enabled[PostBind4] = true      // KPR 모드에서만
    }
    if option.Config.EnableHealthDatapath {
        enabled[PreBind4] = true       // 헬스 데이터패스에서만
    }
}

// SockRelease는 IPv4 또는 IPv6 중 하나라도 활성화되면 필요
enabled[SockRelease] = option.Config.EnableIPv4 || option.Config.EnableIPv6
```

---

## 7. 설계 결정의 이유 (Why)

### Q1: 왜 패킷 레벨(TC/XDP) 대신 소켓 레벨에서 LB하는가?

1. **ConnTrack 불필요**: DNAT가 없으므로 역변환용 CT 엔트리 불필요 → 메모리/CPU 절약
2. **localhost 통신**: 소켓 레벨 변환은 localhost(127.0.0.1)에서 자연스럽게 동작
3. **소스 IP 보존**: 패킷 헤더를 수정하지 않으므로 원본 소스 IP 유지
4. **성능**: 패킷 생성 전에 주소를 변환하므로 재전송 비용 없음

### Q2: 왜 cgroup에 부착하는가?

cgroup BPF는 cgroup에 속한 모든 프로세스의 소켓 시스템 콜에 적용된다. Cilium은 루트 cgroup에 부착하여 노드의 모든 Pod에 자동 적용한다.

### Q3: 왜 bpf_link를 선호하면서도 PROG_ATTACH를 유지하는가?

- `bpf_link`: 원자적 교체, 핀으로 프로세스 독립 → 안전한 에이전트 재시작
- `PROG_ATTACH`: 구 버전 호환성, 업그레이드 시 끊김 없는 전환

---

## 8. 한계와 고려사항

| 항목 | 설명 |
|------|------|
| 커널 요구 | cgroup BPF 지원 (커널 4.10+), bpf_link (5.7+) |
| cgroupv2 | cgroupv2 필수 (cgroupv1에서는 제한적) |
| raw socket | raw socket은 connect() 훅을 통과하지 않음 |
| eBPF 제한 | BPF 프로그램 크기/복잡도 제한 |

---

*검증 기준: Cilium 소스코드 `pkg/socketlb/` 디렉토리 직접 분석*
*문서 작성: Claude Code (Opus 4.6)*
