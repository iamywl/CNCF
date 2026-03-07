# PoC: Terraform 모듈 시스템 시뮬레이션

## 개요

Terraform의 모듈 시스템을 시뮬레이션한다.
모듈은 재사용 가능한 인프라 구성 단위로, 변수로 입력을 받고 출력값을 반환한다.
모듈은 다른 모듈을 호출하여 재귀적 모듈 트리를 형성한다.

## 대응하는 Terraform 소스코드

| 이 PoC | Terraform 소스 | 설명 |
|--------|---------------|------|
| `ModuleConfig` | `internal/configs/module.go` | 모듈 설정 |
| `ModuleCall` | `internal/configs/module_call.go` | 모듈 호출 블록 |
| `ModuleInstaller` | `internal/initwd/module_install.go` | 모듈 설치 |
| `Manifest` | `internal/modsdir/manifest.go` | modules.json |
| `BuildModuleTree()` | `internal/configs/config.go` → `BuildConfig()` | 설정 트리 구축 |
| `ModuleSource` | `internal/addrs/module_source.go` | 소스 주소 파싱 |

## 구현 내용

### 1. 모듈 소스 타입
- **Local**: `./modules/network`, `../shared/vpc` (상대 경로)
- **Registry**: `hashicorp/vpc/aws` (Terraform Registry)
- **Git**: `git::https://github.com/...` (Git 저장소)

### 2. 모듈 설치
- `terraform init` 시 모듈을 `.terraform/modules/`에 설치
- 디렉토리 구조: `.terraform/modules/{key}/`
- modules.json 매니페스트에 설치 정보 기록

### 3. 변수 전달 (Parent -> Child)
- 부모 모듈의 `module` 블록에서 자식에게 변수 전달
- 부모의 변수를 참조하여 자식 변수 값 결정
- 기본값이 있으면 전달하지 않아도 사용 가능

### 4. 출력값 전달 (Child -> Parent)
- 자식 모듈의 `output` 블록에서 값을 선언
- 부모에서 `module.{name}.{output_name}`으로 참조

### 5. 재귀적 모듈 트리
- root -> network -> (public_subnet, private_subnet)
- 각 레벨에서 변수 해결과 모듈 설치가 재귀적으로 수행

## 실행 방법

```bash
go run main.go
```

## 모듈 트리 구조

```
root
├── aws_instance.web
└── module.network
    ├── aws_vpc.main
    ├── aws_internet_gateway.main
    ├── module.public_subnet
    │   └── aws_subnet.this
    └── module.private_subnet
        └── aws_subnet.this
```

## 데이터 흐름

```
변수 전달 (위에서 아래):
  root (environment="production")
    → module.network (environment="production")
      → module.public_subnet (name="production-public")
      → module.private_subnet (name="production-private")

출력값 전달 (아래에서 위):
  module.public_subnet (subnet_id)
    → module.network (public_subnet_id = module.public_subnet.subnet_id)
      → root (public_subnet_id = module.network.public_subnet_id)
```

## 핵심 포인트

- 모듈은 DRY(Don't Repeat Yourself) 원칙을 인프라 코드에 적용하는 핵심 도구이다
- `terraform init`이 모듈을 재귀적으로 설치하고 매니페스트에 기록한다
- 변수와 출력값이 모듈 간 데이터 흐름의 유일한 인터페이스이다
- 모듈 내부 리소스는 캡슐화되어 외부에서 직접 접근할 수 없다
