# PoC-06: Shared/Bridged/Softnet 네트워크 추상화 시뮬레이션

## 개요

tart의 세 가지 네트워크 모드(Shared, Bridged, Softnet)를 Go 표준 라이브러리만으로 재현한다.
Network 프로토콜(인터페이스)을 통해 동일한 API로 다양한 네트워크 구현체를 교체할 수 있는
전략 패턴을 시뮬레이션하며, 이더넷 프레임 수준의 패킷 전달을 데모한다.

## 실행 방법

```bash
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다. `net.Pipe()`로 Softnet의 socketpair를 시뮬레이션한다.

## 핵심 시뮬레이션 포인트

### 1. Network 인터페이스 (tart Network.swift)
- `Attachments()`: VM에 연결할 네트워크 디바이스 반환
- `Run()`: 네트워크 프로세스 시작 (Softnet만 실제 동작)
- `Stop()`: 네트워크 프로세스 종료

### 2. NetworkShared (tart NetworkShared.swift)
- `VZNATNetworkDeviceAttachment` 시뮬레이션
- VM에게 내부 IP(192.168.64.x) 할당, 외부 통신 시 호스트 IP로 NAT 변환
- Run/Stop은 no-op

### 3. NetworkBridged (tart NetworkBridged.swift)
- `VZBridgedNetworkDeviceAttachment` 시뮬레이션
- 다수의 호스트 인터페이스에 브리지 연결
- NAT 없이 L2 직접 참여
- Run/Stop은 no-op

### 4. Softnet (tart Softnet.swift)
- `socketpair(AF_UNIX, SOCK_DGRAM)` -> `net.Pipe()` 시뮬레이션
- VM측 FD + Softnet 프로세스측 FD
- goroutine으로 패킷 포워딩 (tart: 외부 softnet 바이너리)
- 소켓 버퍼 크기: SO_RCVBUF=4MB, SO_SNDBUF=1MB
- SUID 비트 설정 흐름 시뮬레이션

### 5. 이더넷 프레임
- MAC 주소, EtherType(IPv4/ARP), Payload 기반 간소화 프레임
- VM->Host, Host->VM 양방향 패킷 전달 데모

## tart 실제 소스코드 참조

| 파일 | 역할 |
|------|------|
| `Sources/tart/Network/Network.swift` | Network 프로토콜: attachments, run, stop |
| `Sources/tart/Network/NetworkShared.swift` | VZNATNetworkDeviceAttachment 기반 NAT |
| `Sources/tart/Network/NetworkBridged.swift` | VZBridgedNetworkDeviceAttachment 기반 브리지 |
| `Sources/tart/Network/Softnet.swift` | socketpair + 외부 프로세스, SUID 설정 |

## 아키텍처

```
          Network (인터페이스)
         /         |          \
NetworkShared  NetworkBridged  Softnet
  (NAT)        (브리지)        (소켓 페어)
    |              |              |
VZNATAttach   VZBridgedAttach  VZFileHandleAttach
    |              |              |
  macOS NAT    물리 인터페이스  net.Pipe() / socketpair
```

```
Softnet 패킷 흐름:
  VM <--vmFD--> [socketpair] <--softnetFD--> softnet 프로세스 <--> 호스트 네트워크
```
