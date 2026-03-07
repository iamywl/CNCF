# 10. State Management Deep-Dive

## 목차

1. [개요](#1-개요)
2. [상태 계층 구조](#2-상태-계층-구조)
3. [State 최상위 구조체](#3-state-최상위-구조체)
4. [Module 상태](#4-module-상태)
5. [Resource와 ResourceInstance](#5-resource와-resourceinstance)
6. [ResourceInstanceObjectSrc - 직렬화 표현](#6-resourceinstanceobjectsrc---직렬화-표현)
7. [Deposed 인스턴스 메커니즘](#7-deposed-인스턴스-메커니즘)
8. [SyncState - 동시성 안전 래퍼](#8-syncstate---동시성-안전-래퍼)
9. [상태 파일 직렬화 (v4 형식)](#9-상태-파일-직렬화-v4-형식)
10. [상태 잠금과 Lineage](#10-상태-잠금과-lineage)
11. [Drift Detection](#11-drift-detection)
12. [설계 철학과 Why](#12-설계-철학과-why)
13. [요약](#13-요약)

---

## 1. 개요

Terraform의 State Management는 인프라의 "실제 상태"를 추적하는 핵심 서브시스템이다. 상태 파일은 Terraform이 관리하는 리소스의 현재 속성, 의존성, Provider 정보를 저장하여 다음 세 가지 핵심 기능을 수행한다:

1. **매핑**: 설정의 리소스 주소와 실제 인프라 객체를 연결
2. **Diff 계산**: 현재 상태와 기대 상태의 차이를 계산하여 Plan 생성
3. **메타데이터 보존**: 리소스 간 의존성, Provider 설정, 스키마 버전 등 추적

```
+-------------------------------------------------------------+
|                        State                                 |
|  +-----------+  +--------------------+  +-----------+        |
|  | Modules   |  | RootOutputValues   |  | CheckRes  |        |
|  | map       |  | map                |  |           |        |
|  +-----+-----+  +--------------------+  +-----------+        |
|        |                                                     |
|   +----+----+                                                |
|   | Module  |  (addrs.ModuleInstance → *Module)               |
|   +----+----+                                                |
|        |                                                     |
|   +----+-----+                                               |
|   | Resource |  (addrs.Resource → *Resource)                  |
|   +----+-----+                                               |
|        |                                                     |
|   +----+---------+                                           |
|   | ResourceInst |  (addrs.InstanceKey → *ResourceInstance)   |
|   +----+---------+                                           |
|        |                                                     |
|   +----+-------------------+                                 |
|   | ResourceInstObjectSrc |  (Current + Deposed)              |
|   +------------------------+                                 |
+-------------------------------------------------------------+
```

**핵심 소스 파일 위치**:
- `internal/states/state.go` - State 최상위 구조체
- `internal/states/module.go` - Module 상태
- `internal/states/resource.go` - Resource, ResourceInstance
- `internal/states/instance_object_src.go` - ResourceInstanceObjectSrc
- `internal/states/sync.go` - SyncState (동시성 안전 래퍼)
- `internal/states/statefile/version4.go` - v4 형식 직렬화

---

## 2. 상태 계층 구조

### 2.1 4단계 계층

```
State (최상위)
 │
 ├── Module (모듈 인스턴스별)
 │    │
 │    ├── Resource (리소스별)
 │    │    │
 │    │    ├── ResourceInstance (인스턴스별, count/for_each)
 │    │    │    │
 │    │    │    ├── Current: *ResourceInstanceObjectSrc
 │    │    │    │    (현재 원격 객체)
 │    │    │    │
 │    │    │    └── Deposed: map[DeposedKey]*ResourceInstanceObjectSrc
 │    │    │         (교체 대기 중인 이전 객체들)
 │    │    │
 │    │    └── ResourceInstance[1]  (count > 1인 경우)
 │    │         └── ...
 │    │
 │    └── Resource (다른 리소스)
 │
 ├── Module (다른 모듈 인스턴스)
 │
 └── RootOutputValues (루트 모듈 출력 값)
```

### 2.2 주소 체계

```
실제 인프라 매핑 예시:

설정:
  module "vpc" {
    count  = 2
    source = "./modules/vpc"
  }
  # 모듈 내부:
  resource "aws_subnet" "main" {
    for_each = var.subnets
  }

상태 주소:
  module.vpc[0].aws_subnet.main["public"]
  module.vpc[0].aws_subnet.main["private"]
  module.vpc[1].aws_subnet.main["public"]
  module.vpc[1].aws_subnet.main["private"]

계층 구조:
  State
  ├── Module["module.vpc[0]"]
  │    └── Resource["aws_subnet.main"]
  │         ├── Instance[StringKey("public")]
  │         │    └── Current: {AttrsJSON: ..., ID: "subnet-abc123"}
  │         └── Instance[StringKey("private")]
  │              └── Current: {AttrsJSON: ..., ID: "subnet-def456"}
  ├── Module["module.vpc[1]"]
  │    └── Resource["aws_subnet.main"]
  │         ├── Instance[StringKey("public")]
  │         └── Instance[StringKey("private")]
  └── Module[""]  (루트 모듈)
```

---

## 3. State 최상위 구조체

### 3.1 State 정의

```go
// internal/states/state.go
type State struct {
    // 모듈 인스턴스별 상태. 키는 구현 상세이며 외부에서 직접 사용 금지
    Modules map[string]*Module

    // 루트 모듈의 출력 값. 다른 모듈의 출력은 실행 간 지속되지 않음
    RootOutputValues map[string]*OutputValue

    // 가장 최근 업데이트의 체크 결과 스냅샷
    CheckResults *CheckResults
}
```

### 3.2 핵심 메서드

```go
// 빈 상태 생성 (루트 모듈 포함)
func NewState() *State {
    modules := map[string]*Module{}
    modules[addrs.RootModuleInstance.String()] = NewModule(addrs.RootModuleInstance)
    return &State{
        Modules:          modules,
        RootOutputValues: make(map[string]*OutputValue),
    }
}

// 상태가 비어있는지 확인
func (s *State) Empty() bool {
    if s == nil { return true }
    if len(s.RootOutputValues) != 0 { return false }
    for _, ms := range s.Modules {
        if !ms.empty() { return false }
    }
    return true
}

// 모듈 조회
func (s *State) Module(addr addrs.ModuleInstance) *Module {
    return s.Modules[addr.String()]
}

// 모듈 확보 (없으면 생성)
func (s *State) EnsureModule(addr addrs.ModuleInstance) *Module {
    ms := s.Module(addr)
    if ms == nil {
        ms = NewModule(addr)
        s.Modules[addr.String()] = ms
    }
    return ms
}

// 동시성 안전 래퍼 생성
func (s *State) SyncWrapper() *SyncState {
    return &SyncState{state: s, writable: true}
}
```

### 3.3 BuildState 헬퍼 (테스트용)

```go
// 테스트에서 상태를 명령형으로 구축
func BuildState(cb func(*SyncState)) *State {
    s := NewState()
    cb(s.SyncWrapper())
    return s
}

// 사용 예:
state := states.BuildState(func(ss *states.SyncState) {
    ss.SetResourceInstanceCurrent(
        addrs.AbsResourceInstance{...},
        &states.ResourceInstanceObjectSrc{
            AttrsJSON: []byte(`{"id":"i-123","ami":"ami-456"}`),
            Status:    states.ObjectReady,
        },
        providerAddr,
    )
})
```

---

## 4. Module 상태

### 4.1 Module 구조체

```go
// internal/states/module.go
type Module struct {
    Addr      addrs.ModuleInstance       // 모듈 인스턴스 주소
    Resources map[string]*Resource       // 리소스 맵 (키: 리소스 주소 문자열)
}
```

### 4.2 핵심 메서드

```go
// 리소스 조회
func (ms *Module) Resource(addr addrs.Resource) *Resource {
    return ms.Resources[addr.String()]
}

// 리소스 인스턴스 조회
func (ms *Module) ResourceInstance(addr addrs.ResourceInstance) *ResourceInstance {
    rs := ms.Resource(addr.Resource)
    if rs == nil { return nil }
    return rs.Instance(addr.Key)
}

// Provider 설정과 함께 리소스 인스턴스의 현재 객체 저장
func (ms *Module) SetResourceInstanceCurrent(
    addr addrs.ResourceInstance,
    obj *ResourceInstanceObjectSrc,
    provider addrs.AbsProviderConfig) {

    rs := ms.Resource(addr.Resource)

    // obj가 nil이면 인스턴스 정리
    if obj == nil && rs != nil {
        is := rs.Instance(addr.Key)
        if is != nil {
            is.Current = obj
            if !is.HasObjects() {
                delete(rs.Instances, addr.Key)
                if len(rs.Instances) == 0 {
                    delete(ms.Resources, addr.Resource.String())
                }
            }
        }
        return
    }

    // rs가 없으면 새로 생성
    if rs == nil && obj != nil {
        ms.SetResourceProvider(addr.Resource, provider)
        rs = ms.Resource(addr.Resource)
    }

    // 인스턴스 확보 후 현재 객체 설정
    is := rs.EnsureInstance(addr.Key)
    is.Current = obj
    rs.ProviderConfig = provider
}
```

**핵심 패턴: 자동 정리(Pruning)**

상태 트리에서 빈 노드는 자동으로 정리된다:
- 인스턴스에 Current와 Deposed가 모두 없으면 → Instance 삭제
- 리소스에 인스턴스가 없으면 → Resource 삭제
- 모듈에 리소스가 없으면 → Module 삭제 (SyncState에서)

---

## 5. Resource와 ResourceInstance

### 5.1 Resource 구조체

```go
// internal/states/resource.go
type Resource struct {
    Addr           addrs.AbsResource          // 절대 리소스 주소
    Instances      map[addrs.InstanceKey]*ResourceInstance  // 인스턴스 맵
    ProviderConfig addrs.AbsProviderConfig     // Provider 설정 주소
}
```

### 5.2 ResourceInstance 구조체

```go
// internal/states/resource.go
type ResourceInstance struct {
    // Current: 현재 원격 객체를 나타내는 상태
    Current *ResourceInstanceObjectSrc

    // Deposed: create_before_destroy로 인해 교체 대기 중인 이전 객체들
    Deposed map[DeposedKey]*ResourceInstanceObjectSrc
}
```

### 5.3 InstanceKey 종류

```
InstanceKey는 count/for_each에 의해 결정:

count 사용 시:
  resource "aws_instance" "web" { count = 3 }
  → IntKey(0), IntKey(1), IntKey(2)

for_each 사용 시:
  resource "aws_instance" "web" { for_each = { a = 1, b = 2 } }
  → StringKey("a"), StringKey("b")

count/for_each 미사용 시:
  resource "aws_instance" "web" { }
  → NoKey (nil)
```

---

## 6. ResourceInstanceObjectSrc - 직렬화 표현

### 6.1 구조체 정의

```go
// internal/states/instance_object_src.go
type ResourceInstanceObjectSrc struct {
    SchemaVersion uint64          // 리소스 스키마 버전 (마이그레이션용)
    AttrsJSON     []byte          // JSON 인코딩된 속성 값
    AttrsFlat     map[string]string  // 레거시 flatmap 형식 (v3 이하)

    IdentitySchemaVersion uint64  // Identity 스키마 버전
    IdentityJSON          []byte  // Identity JSON

    AttrSensitivePaths []cty.Path  // 민감 속성 경로 배열
    Private            []byte      // Provider 비공개 데이터
    Status             ObjectStatus // 객체 상태 (Ready, Tainted 등)
    Dependencies       []addrs.ConfigResource  // 의존성 목록
    CreateBeforeDestroy bool       // CBD 모드 플래그
}
```

### 6.2 ObjectStatus

```go
type ObjectStatus byte

const (
    ObjectReady   ObjectStatus = 'R'  // 정상 상태
    ObjectTainted ObjectStatus = 'T'  // 오염됨 (재생성 필요)
    ObjectPlanned ObjectStatus = 'P'  // 계획됨 (아직 적용 안됨)
)
```

```
ObjectStatus 전이 다이어그램:

     Plan 완료        Apply 성공
  +-----------+    +-----------+
  |  Planned  |--->|   Ready   |
  +-----------+    +-----+-----+
                         |
                    Apply 실패
                    (Provisioner 등)
                         |
                   +-----+-----+
                   |  Tainted  |
                   +-----------+
                         |
                    다음 Plan에서
                    교체 계획 생성
                         |
                   +-----------+
                   |  Planned  |
                   +-----------+
```

### 6.3 AttrsJSON과 AttrsFlat

```
AttrsJSON (현재 형식, v4):
  {"id":"i-123","ami":"ami-456","instance_type":"t2.micro","tags":{"Name":"web"}}

AttrsFlat (레거시 형식, v3):
  {
    "id":            "i-123",
    "ami":           "ami-456",
    "instance_type": "t2.micro",
    "tags.%":        "1",
    "tags.Name":     "web",
  }

AttrsJSON vs AttrsFlat:
  - AttrsJSON은 타입 정보를 보존 (숫자, 불리언, 중첩 객체)
  - AttrsFlat은 모든 것을 문자열로 평탄화
  - 새 상태는 항상 AttrsJSON 사용
  - AttrsFlat은 업그레이드 시 AttrsJSON으로 변환
```

### 6.4 Decode 메서드

```go
// internal/states/instance_object_src.go
func (os *ResourceInstanceObjectSrc) Decode(schema providers.Schema) (
    *ResourceInstanceObject, error) {

    var val cty.Value
    attrsTy := schema.Body.ImpliedType()  // 스키마에서 타입 추론

    switch {
    case os.decodeValueCache != cty.NilVal:
        val = os.decodeValueCache          // 캐시 히트

    case os.AttrsFlat != nil:
        // 레거시 flatmap → cty.Value 변환
        val, _ = hcl2shim.HCL2ValueFromFlatmap(os.AttrsFlat, attrsTy)

    default:
        // JSON → cty.Value 변환
        val, _ = ctyjson.Unmarshal(os.AttrsJSON, attrsTy)
        // 민감 속성 마킹
        val = marks.MarkPaths(val, marks.Sensitive, os.AttrSensitivePaths)
    }

    return &ResourceInstanceObject{
        Value:               val,
        Identity:            identity,
        Status:              os.Status,
        Dependencies:        os.Dependencies,
        Private:             os.Private,
        CreateBeforeDestroy: os.CreateBeforeDestroy,
    }, nil
}
```

### 6.5 Dependencies 필드

```go
Dependencies []addrs.ConfigResource
```

```
Dependencies는 Terraform이 자동 감지한 리소스 간 의존성:

resource "aws_vpc" "main" { ... }
resource "aws_subnet" "main" {
    vpc_id = aws_vpc.main.id   ← 자동 감지된 의존성
}

상태 파일에 저장:
  aws_subnet.main의 Dependencies = [aws_vpc.main]

왜 상태에 의존성을 저장하는가?
  1. 설정이 삭제된 후에도 파괴 순서를 결정하기 위해
  2. terraform destroy 시 올바른 역순 파괴 보장
  3. 설정 없이 상태만으로 의존성 그래프 구축 가능
```

---

## 7. Deposed 인스턴스 메커니즘

### 7.1 Deposed의 목적

`create_before_destroy` 라이프사이클에서 리소스를 교체할 때:

```
일반 교체 (destroy → create):
  1. 기존 리소스 파괴
  2. 새 리소스 생성
  → 중간에 서비스 중단 발생

create_before_destroy 교체:
  1. 기존 리소스를 "deposed"로 표시
  2. 새 리소스 생성 (Current에 저장)
  3. 새 리소스가 정상이면 deposed 파괴
  → 무중단 교체 가능
```

### 7.2 Deposed 작업 흐름

```
시간 →

1. 초기 상태:
   Instance {
     Current: {id: "old-123", ...}
     Deposed: {}
   }

2. CBD 교체 시작 - 기존 객체를 Depose:
   Instance {
     Current: nil
     Deposed: { "abc12345": {id: "old-123", ...} }
   }

3. 새 객체 생성:
   Instance {
     Current: {id: "new-456", ...}
     Deposed: { "abc12345": {id: "old-123", ...} }
   }

4. Deposed 객체 파괴:
   Instance {
     Current: {id: "new-456", ...}
     Deposed: {}
   }
```

### 7.3 DeposedKey 생성

```go
// internal/states/resource.go
func (i *ResourceInstance) deposeCurrentObject(forceKey DeposedKey) DeposedKey {
    if !i.HasCurrent() {
        return NotDeposed
    }

    key := forceKey
    if key == NotDeposed {
        key = i.findUnusedDeposedKey()  // 랜덤 키 생성
    }

    i.Deposed[key] = i.Current  // 현재 객체를 Deposed로 이동
    i.Current = nil              // 현재 슬롯 비움
    return key
}

// 미사용 DeposedKey 찾기 (32비트 랜덤)
func (i *ResourceInstance) findUnusedDeposedKey() DeposedKey {
    for {
        key := NewDeposedKey()           // 랜덤 생성
        if _, exists := i.Deposed[key]; !exists {
            return key                    // 충돌 없으면 사용
        }
        // 충돌 시 재시도 (32비트 공간에서 거의 발생하지 않음)
    }
}
```

### 7.4 왜 Deposed 맵을 사용하는가?

```
단일 deposed 슬롯이 아닌 맵인 이유:

이론적으로 여러 CBD 교체가 중첩될 수 있다:
  1. Plan: old → new1 (old deposed)
  2. Apply 중 new1 생성 성공, old 파괴 실패
  3. 다음 Plan: new1 → new2 (new1 deposed)
  4. 이 시점에서 old과 new1이 모두 deposed

  Instance {
    Current: new2
    Deposed: {
      "abc12345": old,
      "def67890": new1,
    }
  }

실제로는 드문 상황이지만, 맵 구조로 안전하게 처리.
```

---

## 8. SyncState - 동시성 안전 래퍼

### 8.1 SyncState 구조체

```go
// internal/states/sync.go
type SyncState struct {
    state    *State      // 래핑된 실제 State
    writable bool        // 쓰기 가능 여부
    lock     sync.RWMutex // 읽기-쓰기 잠금
}
```

### 8.2 읽기 패턴 (RLock)

```go
// Module 조회 - 스냅샷 반환
func (s *SyncState) Module(addr addrs.ModuleInstance) *Module {
    s.lock.RLock()
    ret := s.state.Module(addr).DeepCopy()  // 깊은 복사!
    s.lock.RUnlock()
    return ret
}

// Resource 조회
func (s *SyncState) Resource(addr addrs.AbsResource) *Resource {
    s.lock.RLock()
    ret := s.state.Resource(addr).DeepCopy()
    s.lock.RUnlock()
    return ret
}

// ResourceInstance 조회
func (s *SyncState) ResourceInstance(
    addr addrs.AbsResourceInstance) *ResourceInstance {
    s.lock.RLock()
    ret := s.state.ResourceInstance(addr).DeepCopy()
    s.lock.RUnlock()
    return ret
}
```

**핵심: 읽기 시 항상 DeepCopy**

```
왜 DeepCopy를 하는가?

1. 호출자가 반환된 값을 자유롭게 수정할 수 있도록
2. 락을 해제한 후에도 안전하게 사용 가능
3. 원본 State의 무결성 보호

단점:
  - 대형 모듈의 경우 복사 비용 발생
  - 하위 수준 접근자 사용 권장 (Module 전체보다 Instance 단위)
```

### 8.3 쓰기 패턴 (beginWrite)

```go
// beginWrite는 쓰기 작업 시작을 표시하는 내부 메서드
func (s *SyncState) beginWrite() func() {
    s.lock.Lock()
    return func() {
        s.lock.Unlock()
    }
}

// 사용 예: SetResourceInstanceCurrent
func (s *SyncState) SetResourceInstanceCurrent(
    addr addrs.AbsResourceInstance,
    obj *ResourceInstanceObjectSrc,
    provider addrs.AbsProviderConfig) {

    defer s.beginWrite()()  // Lock 획득, defer로 Unlock 보장

    ms := s.state.EnsureModule(addr.Module)
    ms.SetResourceInstanceCurrent(addr.Resource, obj, provider)
    s.maybePruneModule(addr.Module)  // 빈 모듈 자동 정리
}
```

### 8.4 Depose 원자적 연산

```go
func (s *SyncState) DeposeResourceInstanceObject(
    addr addrs.AbsResourceInstance) DeposedKey {

    defer s.beginWrite()()  // 원자적 연산 보장

    ms := s.state.Module(addr.Module)
    if ms == nil { return NotDeposed }

    is := ms.ResourceInstance(addr.Resource)
    if is == nil { return NotDeposed }

    return is.deposeCurrentObject(NotDeposed)
    // DeposedKey 할당과 Current→Deposed 이동이 원자적
}
```

### 8.5 RWMutex 선택 이유

```
RWMutex vs Mutex:

Graph Walk 중 상태 접근 패턴:
  - 읽기: 매우 빈번 (의존성 확인, 참조 해석 등)
  - 쓰기: 상대적으로 드묾 (Apply 완료 시)

RWMutex 장점:
  - 여러 읽기가 동시 가능 → 병렬 Graph Walk 성능 향상
  - 쓰기 시에만 배타적 잠금

Graph Walk 예시 (10개 리소스 병렬):
  Mutex:    R-R-R-R-R-R-R-R-W-R  (직렬화)
  RWMutex:  RRRRRRRR-W-RRRRRRRR  (읽기 병렬)
```

---

## 9. 상태 파일 직렬화 (v4 형식)

### 9.1 v4 JSON 구조

```json
{
  "version": 4,
  "terraform_version": "1.9.0",
  "serial": 42,
  "lineage": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "outputs": {
    "instance_ip": {
      "value": "10.0.1.100",
      "type": "string",
      "sensitive": false
    }
  },
  "resources": [
    {
      "mode": "managed",
      "type": "aws_instance",
      "name": "web",
      "provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
      "instances": [
        {
          "schema_version": 1,
          "attributes": {
            "id": "i-1234567890abcdef0",
            "ami": "ami-0c55b159cbfafe1f0",
            "instance_type": "t2.micro",
            "tags": {
              "Name": "web-server"
            }
          },
          "sensitive_attributes": [
            [{"type":"get_attr","value":"password"}]
          ],
          "private": "eyJzY2hlbWFfdmVyc2lvbiI6IjEifQ==",
          "dependencies": [
            "aws_security_group.web",
            "aws_subnet.main"
          ],
          "create_before_destroy": false
        }
      ]
    },
    {
      "module": "module.vpc",
      "mode": "managed",
      "type": "aws_vpc",
      "name": "main",
      "provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
      "instances": [
        {
          "index_key": 0,
          "attributes": { "id": "vpc-123", "cidr_block": "10.0.0.0/16" }
        }
      ]
    }
  ],
  "check_results": null
}
```

### 9.2 v4 메타데이터 필드

| 필드 | 타입 | 설명 |
|------|------|------|
| `version` | int | 상태 형식 버전 (항상 4) |
| `terraform_version` | string | 상태를 마지막으로 쓴 Terraform 버전 |
| `serial` | uint64 | 상태 일련번호 (변경될 때마다 증가) |
| `lineage` | string | UUID, 상태 계보 식별자 |
| `outputs` | object | 루트 모듈 출력 값 |
| `resources` | array | 리소스 상태 배열 |

### 9.3 readStateV4 구현

```go
// internal/states/statefile/version4.go
func readStateV4(src []byte) (*File, tfdiags.Diagnostics) {
    sV4 := &stateV4{}
    err := json.Unmarshal(src, sV4)
    ...
    file, prepDiags := prepareStateV4(sV4)
    return file, diags
}

func prepareStateV4(sV4 *stateV4) (*File, tfdiags.Diagnostics) {
    // 메타데이터 설정
    file := &File{
        TerraformVersion: tfVersion,
        Serial:           sV4.Serial,
        Lineage:          sV4.Lineage,
    }

    state := states.NewState()

    // 각 리소스 순회
    for _, rsV4 := range sV4.Resources {
        rAddr := addrs.Resource{
            Type: rsV4.Type,
            Name: rsV4.Name,
        }
        // mode 결정
        switch rsV4.Mode {
        case "managed": rAddr.Mode = addrs.ManagedResourceMode
        case "data":    rAddr.Mode = addrs.DataResourceMode
        }

        // 모듈 주소 파싱
        moduleAddr := addrs.RootModuleInstance
        if rsV4.Module != "" {
            moduleAddr, _ = addrs.ParseModuleInstanceStr(rsV4.Module)
        }

        // Provider 주소 파싱
        providerAddr, _ := addrs.ParseAbsProviderConfigStr(rsV4.ProviderConfig)

        ms := state.EnsureModule(moduleAddr)
        ms.SetResourceProvider(rAddr, providerAddr)

        // 각 인스턴스 순회
        for _, isV4 := range rsV4.Instances {
            // InstanceKey 결정 (int, string, 또는 nil)
            var key addrs.InstanceKey
            switch tk := isV4.IndexKey.(type) {
            case float64: key = addrs.IntKey(int(tk))
            case string:  key = addrs.StringKey(tk)
            default:      key = addrs.NoKey
            }

            // ResourceInstanceObjectSrc 구성
            obj := &states.ResourceInstanceObjectSrc{
                SchemaVersion:       isV4.SchemaVersion,
                AttrsJSON:           isV4.AttributesRaw,
                CreateBeforeDestroy: isV4.CreateBeforeDestroy,
            }
            ...
            // Current 또는 Deposed로 설정
            if isV4.DeposedKey == "" {
                ms.SetResourceInstanceCurrent(instAddr, obj, providerAddr)
            } else {
                ms.SetResourceInstanceDeposed(instAddr, dk, obj, providerAddr)
            }
        }
    }
    ...
}
```

### 9.4 상태 형식 버전 히스토리

| 버전 | Terraform | 주요 변경 |
|------|-----------|---------|
| v1 | 0.1~0.3 | 초기 형식 |
| v2 | 0.6.x | 모듈 지원 |
| v3 | 0.7~0.11 | flatmap 기반, Provider 경로 |
| v4 | 0.12+ | JSON 속성, Provider FQN, Lineage |

---

## 10. 상태 잠금과 Lineage

### 10.1 상태 잠금 (State Locking)

```
상태 잠금의 필요성:

팀 A: terraform apply    팀 B: terraform apply
  |  상태 읽기              |  상태 읽기
  |  Plan 계산              |  Plan 계산
  |  변경 적용              |  변경 적용
  |  상태 쓰기 ←→ 충돌! ←→  |  상태 쓰기
  v                         v

잠금 적용 후:
팀 A: terraform apply    팀 B: terraform apply
  |  잠금 획득              |  잠금 시도 → 실패!
  |  상태 읽기              |  "Error: state locked"
  |  Plan 계산              |
  |  변경 적용              |
  |  상태 쓰기              |
  |  잠금 해제              |
  v                         |  잠금 획득
                            |  상태 읽기
                            |  ...
```

### 10.2 Lineage와 Serial

```
Lineage: 상태 "계보"를 식별하는 UUID
  - terraform init 시 최초 생성
  - 같은 인프라를 관리하는 상태들은 같은 Lineage
  - 다른 Lineage의 상태를 덮어쓰려면 -force 필요

Serial: 상태 변경 일련번호
  - 매 상태 쓰기 시 증가
  - 낙관적 동시성 제어에 사용
  - Serial이 뒤로 가면 거부 (오래된 상태로 덮어쓰기 방지)

예시:
  Serial=10 읽기 → Apply → Serial=11 쓰기
  다른 사람이 Serial=12로 이미 업데이트했다면?
  → 쓰기 실패, 최신 상태 다시 읽기 필요
```

### 10.3 원격 백엔드의 잠금 구현

```
Backend별 잠금 메커니즘:

| Backend | 잠금 방식 |
|---------|---------|
| S3      | DynamoDB 테이블 |
| GCS     | 객체 메타데이터 |
| Azure   | Blob 임대(Lease) |
| Consul  | KV 잠금 |
| local   | 파일 시스템 flock |
```

---

## 11. Drift Detection

### 11.1 Drift란?

```
Drift: Terraform 상태와 실제 인프라 사이의 불일치

발생 원인:
1. 수동 변경 (콘솔/CLI로 직접 수정)
2. 다른 도구가 인프라 수정
3. 클라우드 자동 업데이트
4. 외부 이벤트 (리소스 삭제 등)
```

### 11.2 Drift 감지 흐름

```
terraform plan 실행 시:

1. Refresh 단계:
   상태 파일의 각 리소스에 대해 ReadResource() 호출
   → 실제 인프라 상태를 가져옴

2. 비교:
   상태 파일의 이전 속성 vs ReadResource 결과
   차이가 있으면 = Drift 발생

3. Plan 계산:
   Refresh된 상태 + 설정 파일 → 필요한 변경 계산

예시:
  상태 파일: instance_type = "t2.micro"
  실제 AWS:  instance_type = "t3.small"  (수동 변경)
  설정 파일: instance_type = "t2.micro"

  terraform plan 출력:
  ~ aws_instance.web
      instance_type: "t3.small" → "t2.micro"
      (원래 설정으로 되돌림)
```

### 11.3 Refresh와 State 업데이트

```
Refresh 동작:

func ReadResource(req ReadResourceRequest) ReadResourceResponse {
    // CurrentState에서 리소스 ID 추출
    id := req.CurrentState.GetAttr("id")

    // 클라우드 API 호출
    instance, err := aws.DescribeInstances(id)

    if err != nil || instance == nil {
        // 리소스가 삭제됨 → null 반환
        return ReadResourceResponse{NewState: cty.NilVal}
    }

    // 실제 상태를 cty.Value로 변환
    return ReadResourceResponse{
        NewState: instanceToCtyValue(instance),
    }
}

Refresh 결과:
  null 반환 → 상태에서 리소스 제거 (다음 Plan에서 재생성)
  변경된 값 → 상태 업데이트 (Drift 반영)
  동일한 값 → 변경 없음
```

---

## 12. 설계 철학과 Why

### 12.1 왜 상태를 파일로 저장하는가?

```
상태 저장의 대안들:

1. 매번 API 쿼리 (상태 없음):
   - 모든 Plan마다 전체 인프라 조회 필요
   - 느리고, API 제한에 걸릴 수 있음
   - 리소스 매핑 정보 유실
   - 삭제된 리소스 추적 불가

2. 데이터베이스:
   - 로컬 환경에서 추가 의존성
   - 설정 복잡성 증가

3. 파일 기반 (Terraform 선택):
   - 단순, 이식 가능
   - Git 등으로 버전 관리 가능
   - 원격 백엔드로 확장 가능
   - 인간이 읽을 수 있는 JSON
```

### 12.2 왜 ObjectSrc (직렬화 표현)와 Object (런타임 표현)를 분리하는가?

```
ResourceInstanceObjectSrc (직렬화):
  AttrsJSON []byte           // JSON 바이트
  SchemaVersion uint64       // 스키마 버전

ResourceInstanceObject (런타임):
  Value cty.Value            // 타입 안전한 값
  Status ObjectStatus        // 상태

분리 이유:
  1. 스키마 마이그레이션: Src의 SchemaVersion을 확인하고
     현재 스키마와 다르면 UpgradeResourceState 호출 후 Decode
  2. 지연 디코딩: 필요할 때만 JSON → cty.Value 변환
  3. 효율성: 대부분의 리소스는 Plan 시 변경되지 않으므로
     JSON 디코딩 불필요
```

### 12.3 왜 Dependencies를 상태에 저장하는가?

```
설정에서만 의존성을 읽을 수 없는 경우:

scenario 1: 리소스 A가 리소스 B에 의존
            리소스 A의 설정이 삭제됨
            terraform destroy 시 B를 먼저 파괴하면?
            → A의 파괴가 실패할 수 있음

scenario 2: 모듈이 제거된 경우
            모듈 내 리소스의 설정을 읽을 수 없음
            하지만 올바른 파괴 순서는 필요

상태에 Dependencies를 저장하면:
  - 설정이 없어도 의존성 그래프 구축 가능
  - 올바른 파괴 순서 보장
  - "orphan" 리소스도 안전하게 처리
```

### 12.4 왜 SyncState에서 DeepCopy를 하는가?

```
DeepCopy 없이:
  1. goroutine A가 Module 참조 획득, 잠금 해제
  2. goroutine B가 같은 Module 수정 중
  3. goroutine A가 수정 중인 Module을 읽으면 → 경합 조건!

DeepCopy 적용:
  1. goroutine A가 Module의 독립적 복사본 획득
  2. goroutine B가 원본 Module 수정
  3. goroutine A는 안전한 복사본으로 작업 → 문제 없음

비용 vs 안전:
  - DeepCopy는 비용이 있지만
  - Graph Walk의 병렬 실행에서 데이터 경합 방지 필수
  - 세밀한 접근자(Instance 단위)로 복사 범위 최소화
```

### 12.5 왜 Lineage + Serial 조합인가?

```
Serial만 사용할 경우의 문제:

  프로젝트 A: Serial=1→2→3
  프로젝트 B: Serial=1→2→3  (다른 인프라)

  실수로 프로젝트 B의 상태를 A에 덮어쓰면?
  → Serial만으로는 감지 불가 (둘 다 Serial=3)

Lineage + Serial:
  프로젝트 A: Lineage="abc", Serial=3
  프로젝트 B: Lineage="xyz", Serial=3

  덮어쓰기 시도 → Lineage 불일치 → 거부!
  → 다른 인프라의 상태를 실수로 덮어쓰는 사고 방지
```

---

## 13. 요약

### 상태 관리 아키텍처 전체 그림

```
+---------------------------------------------------------------+
|                     Terraform Core                             |
|                                                                |
|  context.Plan() / context.Apply()                              |
|       |                                                        |
|       v                                                        |
|  +----------+     +-----------+     +--------+                 |
|  | SyncState| --> | State     | --> | Module | --> Resource    |
|  | (RWMutex)| <-- | (Modules) | <-- |        | <-- Instance   |
|  +----------+     +-----------+     +--------+     ObjectSrc  |
|       |                                                        |
+-------+--------------------------------------------------------+
        |
        v
   +---------+     +----------+     +---------+
   | Backend | --> | statefile | --> | JSON v4 |
   | (S3/GCS)| <-- | (읽기/   | <-- | (파일)  |
   +---------+     |  쓰기)   |     +---------+
                   +----------+

   잠금: DynamoDB / GCS Metadata / Azure Lease / flock
```

### 핵심 설계 결정 요약

| 결정 | 이유 |
|------|------|
| 4단계 계층 (State→Module→Resource→Instance) | 모듈 시스템과 count/for_each 지원 |
| ObjectSrc/Object 분리 | 스키마 마이그레이션, 지연 디코딩 |
| AttrsJSON (v4) | 타입 정보 보존, 구조화된 데이터 |
| Deposed 맵 | create_before_destroy 무중단 교체 |
| SyncState + RWMutex | 병렬 Graph Walk 안전성 |
| DeepCopy 반환 | 잠금 없이 안전한 읽기 |
| Dependencies 상태 저장 | 설정 삭제 후 파괴 순서 보장 |
| Lineage + Serial | 상태 계보 추적, 충돌 감지 |
| 자동 Pruning | 빈 노드 자동 정리로 상태 깨끗 유지 |
