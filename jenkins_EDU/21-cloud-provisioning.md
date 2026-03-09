# 21. Cloud / Auto-Provisioning 시스템 Deep-Dive

## 1. 개요

Jenkins의 Cloud/Auto-Provisioning 시스템은 **빌드 부하에 따라 에이전트 노드를
동적으로 생성하고 해제**하는 메커니즘이다. AWS EC2, Kubernetes, Docker 등
다양한 클라우드 프로바이더를 추상화하여 **탄력적 빌드 인프라**를 구현한다.

### 왜(Why) 이 서브시스템이 존재하는가?

전통적인 Jenkins 환경에서는 고정된 수의 에이전트를 수동으로 등록한다.
이 방식의 문제점:

1. **리소스 낭비**: 빌드가 없을 때도 에이전트 VM이 실행 중 → 비용 낭비
2. **용량 한계**: 갑자기 빌드가 몰리면 고정 에이전트로 감당 불가
3. **수동 관리**: 에이전트 추가/제거에 관리자 개입 필요
4. **환경 일관성**: 수동 관리 에이전트는 시간이 지나면 환경이 달라짐

Cloud Auto-Provisioning은 이 모든 문제를 해결한다:
- **필요할 때만** 에이전트 생성 (비용 최적화)
- **자동 스케일링** (부하 기반)
- **일회용 에이전트** (환경 일관성)
- **API 기반 관리** (자동화)

## 2. 핵심 아키텍처

```
┌───────────────────────────────────────────────────────────────┐
│                    Jenkins 마스터                               │
│                                                               │
│  Queue (대기 중인 빌드)                                         │
│       │                                                       │
│       ▼                                                       │
│  NodeProvisioner (Label별)                                     │
│       │                                                       │
│       ├─→ 부하 분석 (Trend Analysis)                           │
│       │       excessWorkload = 수요 - 현재 용량                 │
│       │                                                       │
│       ├─→ CloudProvisioningListener.canProvision() 검사        │
│       │                                                       │
│       ├─→ Cloud.canProvision(CloudState) 확인                  │
│       │                                                       │
│       └─→ Cloud.provision(CloudState, excessWorkload)          │
│               │                                               │
│               ▼                                               │
│       Collection<PlannedNode>                                  │
│               │                                               │
│               ├─→ Future<Node> (비동기 프로비저닝)               │
│               │       │                                       │
│               │       └─→ Jenkins.addNode(node)               │
│               │                                               │
│               └─→ RetentionStrategy (유휴 시 해제)             │
│                       │                                       │
│                       └─→ AbstractCloudSlave.terminate()      │
└───────────────────────────────────────────────────────────────┘

┌───────────────────────────────────────────────────────────────┐
│                    클라우드 프로바이더                           │
│                                                               │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐      │
│  │   AWS    │  │   K8s    │  │  Docker  │  │  Azure   │      │
│  │   EC2    │  │   Pod    │  │Container │  │    VM    │      │
│  └──────────┘  └──────────┘  └──────────┘  └──────────┘      │
└───────────────────────────────────────────────────────────────┘
```

## 3. 핵심 클래스 분석

### 3.1 Cloud — 클라우드 추상화

**경로**: `core/src/main/java/hudson/slaves/Cloud.java`

`Cloud`는 동적 노드 프로비저닝의 **최상위 추상 클래스**이다.

```java
public abstract class Cloud extends Actionable
    implements ExtensionPoint, Describable<Cloud>, AccessControlled {

    // 고유 이름 (URL 토큰으로도 사용)
    public String name;

    // 이 클라우드가 해당 레이블의 노드를 프로비저닝할 수 있는가?
    public boolean canProvision(CloudState state) {
        return canProvision(state.getLabel());
    }

    // 노드 프로비저닝 실행
    public Collection<PlannedNode> provision(CloudState state, int excessWorkload) {
        return provision(state.getLabel(), excessWorkload);
    }
}
```

#### CloudState — 프로비저닝 컨텍스트

```java
public static final class CloudState {
    @CheckForNull
    private final Label label;           // 필요한 레이블
    private final int additionalPlannedCapacity;  // 이전 전략이 이미 계획한 용량
}
```

`additionalPlannedCapacity`가 존재하는 이유: 여러 Cloud가 등록되어 있을 때,
앞선 Cloud가 이미 계획한 노드 수를 알려줘서 중복 프로비저닝을 방지한다.

#### PROVISION 퍼미션

```java
public static final Permission PROVISION = new Permission(
    Computer.PERMISSIONS, "Provision",
    Messages._Cloud_ProvisionPermission_Description(),
    Jenkins.ADMINISTER, PERMISSION_SCOPE);
```

프로비저닝은 인프라 비용에 직결되므로 별도 퍼미션으로 관리한다.

### 3.2 NodeProvisioner — 부하 분석 엔진

**경로**: `core/src/main/java/hudson/slaves/NodeProvisioner.java`

`NodeProvisioner`는 각 `Label`에 대해 하나씩 존재하며,
큐의 부하를 분석하여 Cloud에 프로비저닝을 요청한다.

#### PlannedNode — 비동기 프로비저닝 결과

```java
public static class PlannedNode {
    public final String displayName;
    public final Future<Node> future;
    public final int numExecutors;
}
```

`Future<Node>`가 핵심이다:
- 프로비저닝은 비동기적으로 진행 (VM 시작에 수 분 소요 가능)
- `future.get()` 완료 시 `Jenkins.addNode(node)` 호출
- 실패 시 `CloudProvisioningListener.onFailure()` 호출

### 3.3 CloudProvisioningListener — 프로비저닝 라이프사이클

**경로**: `core/src/main/java/hudson/slaves/CloudProvisioningListener.java`

프로비저닝 과정의 각 단계에 훅을 제공하는 확장 포인트.

```java
public abstract class CloudProvisioningListener implements ExtensionPoint {
    // 프로비저닝 허용/거부 (null = 허용, non-null = 거부 사유)
    public CauseOfBlockage canProvision(Cloud cloud, CloudState state, int numExecutors);

    // 프로비저닝 시작됨 (PlannedNode 생성 직후)
    public void onStarted(Cloud cloud, Label label,
                          Collection<PlannedNode> plannedNodes);

    // 프로비저닝 완료 (Future 성공)
    public void onComplete(PlannedNode plannedNode, Node node);

    // 노드가 Jenkins에 완전히 연결됨
    public void onCommit(PlannedNode plannedNode, Node node);

    // 프로비저닝 실패
    public void onFailure(PlannedNode plannedNode, Throwable t);

    // Jenkins.addNode() 실패 시 롤백
    public void onRollback(PlannedNode plannedNode, Node node, Throwable t);
}
```

#### 라이프사이클 순서

```
canProvision() → [허용]
    → Cloud.provision()
    → onStarted()
    → Future<Node> 실행 중...
    → [성공] onComplete() → Jenkins.addNode() → onCommit()
    → [성공] onComplete() → Jenkins.addNode() 실패 → onRollback()
    → [실패] onFailure()
```

### 3.4 CloudRetentionStrategy — 유휴 에이전트 자동 해제

**경로**: `core/src/main/java/hudson/slaves/CloudRetentionStrategy.java`

클라우드에서 프로비저닝된 에이전트가 일정 시간 유휴 상태면 자동 종료한다.

```java
public class CloudRetentionStrategy extends RetentionStrategy<AbstractCloudComputer> {
    private int idleMinutes;

    @Override
    @GuardedBy("hudson.model.Queue.lock")
    public long check(final AbstractCloudComputer c) {
        final AbstractCloudSlave computerNode = c.getNode();
        if (c.isIdle() && !disabled && computerNode != null) {
            final long idleMilliseconds =
                System.currentTimeMillis() - c.getIdleStartMilliseconds();
            if (idleMilliseconds > MINUTES.toMillis(idleMinutes)) {
                LOGGER.info("Disconnecting " + c.getName());
                computerNode.terminate();  // VM 종료
            }
        }
        return 0;
    }

    @Override
    public void start(AbstractCloudComputer c) {
        c.connect(false);  // ASAP 연결 시도
    }
}
```

**설계 결정**:
- `@GuardedBy("hudson.model.Queue.lock")`: Queue 잠금 하에서 실행되므로
  check 중 새 빌드가 할당되는 것을 방지
- `disabled` 플래그: `SystemProperties`로 비활성화 가능 (디버깅용)
- `return 0`: 다음 check까지 대기 시간 없음 (RetentionStrategy 프레임워크가 관리)

### 3.5 CloudSlaveRetentionStrategy

```java
public class CloudSlaveRetentionStrategy<T extends AbstractCloudComputer>
    extends CloudRetentionStrategy {
    // CloudRetentionStrategy의 제네릭 버전
}
```

## 4. 프로비저닝 흐름 상세

```mermaid
sequenceDiagram
    participant Queue as Build Queue
    participant NP as NodeProvisioner
    participant CPL as CloudProvisioningListener
    participant Cloud as Cloud 구현체
    participant Provider as 클라우드 프로바이더 (AWS/K8s)

    Queue->>NP: 대기 중인 빌드 감지
    NP->>NP: 부하 분석 (excessWorkload 계산)

    NP->>CPL: canProvision(cloud, state, n)
    CPL-->>NP: null (허용)

    NP->>Cloud: canProvision(state)
    Cloud-->>NP: true

    NP->>Cloud: provision(state, excessWorkload)
    Cloud->>Provider: VM/컨테이너 생성 요청
    Cloud-->>NP: Collection<PlannedNode>

    NP->>CPL: onStarted(cloud, label, plannedNodes)

    Provider-->>Cloud: VM/컨테이너 준비 완료
    Note over Cloud: Future<Node> 완료

    NP->>CPL: onComplete(plannedNode, node)
    NP->>NP: Jenkins.addNode(node)
    NP->>CPL: onCommit(plannedNode, node)

    Note over NP: 에이전트 연결 및 빌드 실행

    Note over NP: 유휴 시간 경과
    NP->>Cloud: CloudRetentionStrategy.check()
    Cloud->>Provider: VM/컨테이너 종료
```

## 5. Cloud 구현 패턴

### 5.1 Cloud 구현 시 필요한 것들

Cloud 플러그인 개발자가 구현해야 할 핵심:

| 컴포넌트 | 역할 |
|----------|------|
| `Cloud` 하위 클래스 | `canProvision()`, `provision()` 구현 |
| `Slave` 하위 클래스 | `createComputer()`, `terminate()` 구현 |
| `Computer` 하위 클래스 | `onRemoved()`에서 리소스 해제 |
| `RetentionStrategy` | `CloudRetentionStrategy` 사용 또는 커스텀 |
| `ComputerLauncher` | 에이전트 연결 방식 (SSH, JNLP 등) |

### 5.2 에이전트 생명주기

```
Cloud.provision()
    │
    ├─→ Future<Node> (비동기)
    │       │
    │       └─→ new MyCloudSlave(instanceId, ...)
    │               │
    │               └─→ Jenkins.addNode(slave)
    │                       │
    │                       ├─→ slave.createComputer()
    │                       │       → new MyCloudComputer(slave)
    │                       │
    │                       └─→ computer.connect(false)
    │                               → ComputerLauncher.launch()
    │
    ├─→ [빌드 실행]
    │
    └─→ RetentionStrategy.check()
            │
            ├─→ [유휴] slave.terminate()
            │       │
            │       ├─→ 클라우드 리소스 해제
            │       └─→ Jenkins.removeNode(slave)
            │               └─→ computer.onRemoved()
            │
            └─→ [사용 중] 대기
```

### 5.3 리소스 해제 설계

**왜 Computer.onRemoved()에서 리소스를 해제하는가?**

Jenkins 소스 코드의 Javadoc에서 이유를 설명한다:

> *Have your Slave subtype remember the necessary handle (such as EC2 instance ID)
> as a field. ... Finally, override Computer.onRemoved() and use the handle to
> talk to the "cloud" and de-allocate the resource.*
>
> *Computer needs to own this handle information because by the time this happens,
> a Slave object is already long gone.*

- `Slave` 객체는 사용자가 재설정(`configure`)할 수 있어 핸들이 유실될 수 있다
- `Computer` 객체는 `Slave`보다 오래 살아남으므로 안전하게 핸들을 유지

## 6. 설정과 관리

### 6.1 Cloud 등록

```
Jenkins 관리 → 노드 관리 → Cloud 설정
```

여러 Cloud를 등록하면 `Jenkins.clouds` 리스트에 순서대로 저장되며,
프로비저닝 시 순서대로 `canProvision()`을 확인한다.

### 6.2 JCasC 설정 예시

```yaml
jenkins:
  clouds:
    - amazonEC2:
        name: "aws-cloud"
        region: "us-east-1"
        templates:
          - ami: "ami-12345678"
            type: "m5.large"
            labels: "linux docker"
            numExecutors: 2
            idleTerminationMinutes: 30
            remoteFS: "/home/jenkins"
```

### 6.3 Cloud 삭제

```java
// Cloud.java
@RequirePOST
public HttpResponse doDoDelete() throws IOException {
    checkPermission(Jenkins.ADMINISTER);
    Jenkins.get().clouds.remove(this);
    return new HttpRedirect("..");
}
```

## 7. Cloud 설정 UI

```
┌─────────────────────────────────────────────┐
│ Cloud: aws-cloud                            │
├─────────────────────────────────────────────┤
│ Name: [aws-cloud        ]                   │
│ Region: [us-east-1  ▼]                      │
│                                             │
│ Templates:                                  │
│ ┌─────────────────────────────────────────┐ │
│ │ AMI: ami-12345678                       │ │
│ │ Type: m5.large                          │ │
│ │ Labels: linux docker                    │ │
│ │ Executors: 2                            │ │
│ │ Idle Timeout: 30 min                    │ │
│ └─────────────────────────────────────────┘ │
│                                             │
│ [Apply] [Save] [Delete Cloud]               │
└─────────────────────────────────────────────┘
```

## 8. 보안

### 8.1 권한 모델

```
Jenkins.ADMINISTER
    └── Cloud.PROVISION
           ├── Cloud 추가/수정/삭제
           └── 수동 프로비저닝 트리거
```

### 8.2 Cloud 설정 변경

```java
// Cloud.doConfigSubmit()
@POST
public HttpResponse doConfigSubmit(StaplerRequest2 req, StaplerResponse2 rsp) {
    checkPermission(Jenkins.ADMINISTER);
    Cloud result = cloud.reconfigure(req, req.getSubmittedForm());
    // 이름 중복 확인
    if (!proposedName.equals(this.name)
        && j.getCloud(proposedName) != null) {
        throw new FormException("Cloud already exists", "name");
    }
    j.clouds.replace(this, result);
    j.save();
}
```

## 9. 확장 포인트 정리

| 확장 포인트 | 역할 | 등록 |
|-------------|------|------|
| `Cloud` | 클라우드 프로바이더 구현 | `@Extension` |
| `CloudProvisioningListener` | 프로비저닝 라이프사이클 훅 | `@Extension` |
| `RetentionStrategy` | 에이전트 유지/해제 전략 | `@Extension` |
| `ComputerLauncher` | 에이전트 연결 방식 | `@Extension` |
| `NodeProvisioner.Strategy` | 프로비저닝 결정 전략 | `@Extension` |

## 10. 기존 Cloud 플러그인

| 플러그인 | 프로바이더 | 에이전트 유형 |
|----------|----------|-------------|
| `ec2` | AWS EC2 | VM (AMI) |
| `kubernetes` | Kubernetes | Pod |
| `docker-plugin` | Docker | Container |
| `azure-vm-agents` | Azure | VM |
| `google-compute-engine` | GCE | VM |

## 11. 정리

Jenkins Cloud/Auto-Provisioning 시스템의 핵심 설계 원칙:

1. **추상화 계층**: `Cloud` 추상 클래스로 모든 클라우드 프로바이더를 통일된 인터페이스로 관리
2. **비동기 프로비저닝**: `Future<Node>`로 VM/컨테이너 생성의 비동기 특성을 자연스럽게 처리
3. **자동 스케일링**: `NodeProvisioner`가 부하를 분석하고 필요한 만큼만 프로비저닝
4. **자동 해제**: `CloudRetentionStrategy`로 유휴 에이전트를 자동 종료하여 비용 최적화
5. **라이프사이클 훅**: `CloudProvisioningListener`로 프로비저닝 과정을 관찰하고 제어
6. **보안**: `Cloud.PROVISION` 퍼미션으로 인프라 비용에 직결되는 작업을 통제
