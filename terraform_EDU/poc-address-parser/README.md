# PoC 11: 주소(Address) 파싱 시뮬레이션

## 개요

Terraform의 주소(Address) 파싱 시스템을 시뮬레이션합니다. Terraform은 리소스, 모듈, 프로바이더를 모두 구조화된 주소로 참조하며, 이 주소 체계는 Plan, State, Graph 등 거의 모든 서브시스템에서 사용됩니다.

## 학습 목표

1. **AbsResourceInstance**: 모듈 경로 + 리소스 + 인스턴스 키로 구성된 절대 주소
2. **InstanceKey**: NoKey, IntKey(count), StringKey(for_each)의 세 가지 키 타입
3. **ModuleInstance**: 모듈 경로 표현 (중첩 모듈 지원)
4. **Provider 주소**: hostname/namespace/type 3단계 정규화
5. **ResourceMode**: managed(resource) vs data(data source) 구분

## Terraform 실제 코드 참조

| 개념 | 실제 파일 |
|------|----------|
| InstanceKey | `internal/addrs/instance_key.go` |
| Resource | `internal/addrs/resource.go` |
| AbsResource | `internal/addrs/resource.go` |
| ModuleInstance | `internal/addrs/module_instance.go` |
| Provider | `internal/addrs/provider.go` |
| 파서 | `internal/addrs/parse_ref.go` |

## 구현 내용

### 주소 종류

| 주소 형식 | 예시 | 설명 |
|-----------|------|------|
| 단순 리소스 | `aws_instance.web` | 타입.이름 |
| 인스턴스 (IntKey) | `aws_instance.web[0]` | count 사용 |
| 인스턴스 (StringKey) | `aws_instance.web["key"]` | for_each 사용 |
| 데이터 소스 | `data.aws_ami.latest` | data 블록 |
| 모듈 리소스 | `module.network.aws_subnet.main` | 모듈 내 리소스 |
| 중첩 모듈 | `module.vpc.module.subnets.aws_subnet.this[0]` | 중첩 모듈 |
| 프로바이더 (단축) | `aws` | hashicorp/aws로 확장 |
| 프로바이더 (정규) | `registry.terraform.io/hashicorp/aws` | 전체 주소 |

### 타입 계층

```
AbsResourceInstance
├── ModuleInstance (경로)
│   └── []ModuleInstanceStep
│       ├── Name (모듈 이름)
│       └── InstanceKey
└── ResourceInstance
    ├── Resource
    │   ├── Mode (managed/data)
    │   ├── Type (리소스 타입)
    │   └── Name (리소스 이름)
    └── InstanceKey
        ├── NoKey
        ├── IntKey (count)
        └── StringKey (for_each)
```

## 실행 방법

```bash
go run main.go
```

## 데모 시나리오

1. 단순 리소스 주소 파싱
2. count/for_each 인스턴스 키 처리
3. 데이터 소스 주소 파싱
4. 모듈 내/중첩 모듈 리소스 주소 파싱
5. 모듈 인스턴스 경로 파싱
6. 프로바이더 주소 파싱 (단축형 → 정규형 변환)
7. 주소 프로그래밍적 구성
8. 파싱 오류 처리
