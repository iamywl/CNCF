# 06. Terraform 운영 가이드

## 1. 설치 및 빌드

### 1.1 바이너리 설치

```bash
# macOS (Homebrew)
brew install hashicorp/tap/terraform

# Linux (apt)
wget -O- https://apt.releases.hashicorp.com/gpg | sudo gpg --dearmor -o /usr/share/keyrings/hashicorp-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/hashicorp-archive-keyring.gpg] https://apt.releases.hashicorp.com $(lsb_release -cs) main" | sudo tee /etc/apt/sources.list.d/hashicorp.list
sudo apt update && sudo apt install terraform

# 버전 확인
terraform version
```

### 1.2 소스에서 빌드

```bash
# Go 설치 필요 (.go-version 파일 참조)
git clone https://github.com/hashicorp/terraform.git
cd terraform
go install .

# 빌드된 바이너리 위치
$(go env GOPATH)/bin/terraform
```

### 1.3 개발 환경

```bash
# 테스트 실행
go test ./...

# 특정 패키지 테스트
go test ./internal/terraform/...
go test ./internal/command/...

# 코드 생성 (protobuf 등)
go generate ./...

# Acceptance 테스트 (외부 서비스 연동)
TF_ACC=1 go test ./internal/initwd
```

## 2. CLI 설정

### 2.1 CLI 설정 파일

```bash
# 위치
~/.terraformrc          # Unix
%APPDATA%/terraform.rc  # Windows

# 또는 환경 변수로 지정
export TERRAFORM_CONFIG_FILE="/path/to/config"
```

### 2.2 설정 예시

```hcl
# ~/.terraformrc

# 프로바이더 플러그인 캐시
plugin_cache_dir = "$HOME/.terraform.d/plugin-cache"

# 프로바이더 설치 소스 설정
provider_installation {
  # 회사 내부 미러 우선
  network_mirror {
    url = "https://terraform-mirror.internal.company.com/"
  }

  # 미러에 없으면 공식 레지스트리
  direct {}
}

# 자격 증명
credentials "app.terraform.io" {
  token = "xxxxx.atlasv1.xxxxxx"
}

# 자격 증명 헬퍼 (1Password, vault 등)
credentials_helper "example" {
  args = ["--config-file", "/path/to/config"]
}
```

## 3. 환경 변수

### 3.1 핵심 환경 변수

| 환경 변수 | 설명 | 예시 |
|-----------|------|------|
| `TF_LOG` | 로그 레벨 설정 | `TRACE`, `DEBUG`, `INFO`, `WARN`, `ERROR` |
| `TF_LOG_PATH` | 로그 파일 경로 | `/tmp/terraform.log` |
| `TF_LOG_CORE` | 코어 로그 레벨 | `TRACE` |
| `TF_LOG_PROVIDER` | 프로바이더 로그 레벨 | `DEBUG` |
| `TF_INPUT` | 입력 프롬프트 비활성화 | `0` (비활성화) |
| `TF_VAR_name` | 변수 값 설정 | `TF_VAR_region=us-west-2` |
| `TF_CLI_ARGS` | 추가 CLI 인자 | `-no-color` |
| `TF_CLI_ARGS_plan` | plan 명령 전용 인자 | `-parallelism=20` |
| `TF_DATA_DIR` | 데이터 디렉토리 | `.terraform` (기본값) |
| `TF_WORKSPACE` | 작업 워크스페이스 | `staging` |
| `TF_IN_AUTOMATION` | 자동화 환경 표시 | `1` (자동화 환경) |

### 3.2 디버깅용 환경 변수

```bash
# 최대 상세도 로그
export TF_LOG=TRACE
export TF_LOG_PATH=/tmp/terraform.log

# 프로바이더만 디버그
export TF_LOG_PROVIDER=DEBUG

# 프로바이더 재연결 (SDK 테스트)
export TF_REATTACH_PROVIDERS='{"registry.terraform.io/hashicorp/aws":{"Protocol":"grpc","ProtocolVersion":5,"Pid":12345,"Test":true,"Addr":{"Network":"unix","String":"/tmp/plugin.sock"}}}'
```

## 4. 워크플로우

### 4.1 기본 워크플로우

```
1. 초기화
   $ terraform init
   - .terraform/ 디렉토리 생성
   - 프로바이더 다운로드
   - 모듈 설치
   - 백엔드 설정

2. 유효성 검사
   $ terraform validate
   - HCL 문법 검사
   - 타입 검사
   - 참조 유효성 확인

3. 포맷팅
   $ terraform fmt
   - HCL 코드 자동 포맷팅

4. 계획
   $ terraform plan -out=plan.tfplan
   - 변경 사항 미리 확인
   - Plan 파일 저장 (선택적)

5. 적용
   $ terraform apply plan.tfplan
   - Plan에 따라 변경 적용

6. 상태 확인
   $ terraform show
   - 현재 State 표시
```

### 4.2 CI/CD 워크플로우

```bash
#!/bin/bash
set -euo pipefail

# CI 환경 표시 (프롬프트 비활성화)
export TF_IN_AUTOMATION=1
export TF_INPUT=0

# 초기화 (백엔드 설정은 환경 변수로)
terraform init -backend-config="bucket=${STATE_BUCKET}" \
               -backend-config="key=${STATE_KEY}" \
               -backend-config="region=${AWS_REGION}"

# Plan 생성 및 저장
terraform plan -out=plan.tfplan -detailed-exitcode
PLAN_EXIT=$?

# Exit code: 0=변경없음, 1=에러, 2=변경있음
if [ $PLAN_EXIT -eq 2 ]; then
    # 변경 사항이 있는 경우만 Apply
    terraform apply -auto-approve plan.tfplan
fi
```

### 4.3 Destroy 워크플로우

```bash
# 전체 삭제
terraform destroy

# 특정 리소스만 삭제
terraform destroy -target=aws_instance.web

# Plan으로 먼저 확인
terraform plan -destroy -out=destroy.tfplan
terraform apply destroy.tfplan
```

## 5. 상태 관리

### 5.1 State 명령

```bash
# State 목록 조회
terraform state list
# 출력:
# aws_vpc.main
# aws_subnet.private["us-east-1a"]
# module.web.aws_instance.server[0]

# 특정 리소스 상세 보기
terraform state show aws_instance.web

# State 이동 (리소스 이름 변경)
terraform state mv aws_instance.old aws_instance.new

# State에서 제거 (실제 리소스는 삭제 안 함)
terraform state rm aws_instance.web

# State 가져오기 (원격 → 로컬)
terraform state pull > state.json

# State 밀어넣기 (로컬 → 원격)
terraform state push state.json

# 프로바이더 교체
terraform state replace-provider hashicorp/aws registry.terraform.io/hashicorp/aws
```

### 5.2 Import

```bash
# 기존 인프라 가져오기
terraform import aws_instance.web i-1234567890abcdef0

# 모듈 내 리소스
terraform import module.web.aws_instance.server i-1234567890abcdef0

# count 인스턴스
terraform import 'aws_instance.web[0]' i-1234567890abcdef0

# for_each 인스턴스
terraform import 'aws_subnet.private["us-east-1a"]' subnet-1234567890
```

### 5.3 State 잠금

```bash
# 잠금 강제 해제 (주의: 다른 프로세스가 실행 중이 아닌지 확인)
terraform force-unlock LOCK_ID

# 잠금 비활성화 (위험)
terraform plan -lock=false

# 잠금 타임아웃
terraform apply -lock-timeout=5m
```

## 6. 백엔드 설정

### 6.1 로컬 백엔드 (기본)

```hcl
terraform {
  backend "local" {
    path = "terraform.tfstate"
  }
}
```

### 6.2 S3 백엔드

```hcl
terraform {
  backend "s3" {
    bucket         = "my-terraform-state"
    key            = "prod/terraform.tfstate"
    region         = "us-east-1"
    encrypt        = true
    dynamodb_table = "terraform-locks"    # 잠금용 DynamoDB 테이블
  }
}
```

### 6.3 GCS 백엔드

```hcl
terraform {
  backend "gcs" {
    bucket = "my-terraform-state"
    prefix = "prod"
  }
}
```

### 6.4 HCP Terraform (Cloud)

```hcl
terraform {
  cloud {
    organization = "my-org"
    workspaces {
      name = "my-workspace"
    }
  }
}
```

### 6.5 백엔드 마이그레이션

```bash
# backend 블록 변경 후
terraform init -migrate-state

# 확인 메시지:
# Do you want to migrate all workspaces to "s3"?
# Enter "yes" to proceed.
```

## 7. 워크스페이스

```bash
# 워크스페이스 목록
terraform workspace list
# * default
#   staging
#   production

# 새 워크스페이스 생성
terraform workspace new staging

# 워크스페이스 전환
terraform workspace select production

# 현재 워크스페이스 확인
terraform workspace show
# production

# 워크스페이스 삭제
terraform workspace delete staging
```

워크스페이스별 State 분리:
```
S3 백엔드의 경우:
s3://bucket/
├── env:/default/terraform.tfstate
├── env:/staging/terraform.tfstate
└── env:/production/terraform.tfstate
```

## 8. 트러블슈팅

### 8.1 로그 확인

```bash
# 최대 상세도 로그
TF_LOG=TRACE terraform plan 2>terraform.log

# 코어와 프로바이더 분리 로깅
TF_LOG_CORE=WARN TF_LOG_PROVIDER=TRACE terraform plan

# 로그 파일에 저장
TF_LOG=DEBUG TF_LOG_PATH=./debug.log terraform apply
```

### 8.2 일반적인 문제 해결

**프로바이더 설치 실패**
```bash
# 캐시 정리 후 재초기화
rm -rf .terraform/providers/
terraform init

# 프로바이더 미러 사용
terraform providers mirror /path/to/mirror
```

**State 잠금 문제**
```bash
# 잠금 정보 확인
terraform plan
# Error: Error acquiring the state lock
# Lock Info:
#   ID:        xxxxxxxxx
#   Path:      s3://bucket/terraform.tfstate
#   Operation: OperationTypePlan
#   Who:       user@hostname

# 프로세스가 이미 종료되었다면 강제 해제
terraform force-unlock xxxxxxxxx
```

**State 불일치 (Drift)**
```bash
# 원격 상태 새로고침
terraform refresh

# 또는 plan에서 refresh 포함
terraform plan -refresh-only
terraform apply -refresh-only
```

**순환 의존성 (Cycle)**
```
Error: Cycle: aws_security_group.a, aws_security_group.b

해결 방법:
1. depends_on 확인 및 제거
2. 리소스 분리 (aws_security_group_rule 사용)
3. terraform graph | dot -Tpng > graph.png 로 시각화
```

**대용량 State 성능**
```bash
# State 크기 확인
terraform state pull | wc -c

# 불필요한 리소스 정리
terraform state rm aws_instance.old_unused

# 모듈 분리 (State 분할)
# 큰 프로젝트를 여러 루트 모듈로 분리
```

### 8.3 그래프 디버깅

```bash
# 의존성 그래프 시각화
terraform graph | dot -Tsvg > graph.svg

# Plan 그래프
terraform graph -type=plan | dot -Tpng > plan-graph.png

# Apply 그래프
terraform graph -type=apply | dot -Tpng > apply-graph.png
```

### 8.4 크래시 디버깅

```
Terraform 크래시 시:
1. crash.log 확인 (자동 생성)
2. TF_LOG=TRACE로 재실행하여 상세 로그 확인
3. State 백업 확인 (terraform.tfstate.backup)
4. GitHub Issues에 보고
```

## 9. 보안 권장사항

### 9.1 State 보안

```
State에는 민감 정보가 포함될 수 있음:
- 데이터베이스 비밀번호
- API 키
- 인증서 내용

보안 조치:
1. 원격 백엔드 사용 (로컬 파일 대신)
2. State 암호화 활성화 (S3: encrypt=true)
3. 접근 제어 설정 (IAM, 버킷 정책)
4. State 파일을 .gitignore에 추가
5. terraform.tfstate.backup도 .gitignore에 추가
```

### 9.2 비밀 관리

```hcl
# 나쁜 예: 하드코딩
provider "aws" {
  access_key = "AKIAXXXXXXXXXXXXXXXX"  # 절대 금지!
  secret_key = "xxxxxxxxxxxxxxxxxxxxxxxx"
}

# 좋은 예: 환경 변수
provider "aws" {
  # AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY 환경 변수 사용
}

# 좋은 예: 변수 파일 (gitignore 처리)
variable "db_password" {
  type      = string
  sensitive = true  # 출력에서 마스킹
}
```

### 9.3 .gitignore 템플릿

```gitignore
# Terraform
*.tfstate
*.tfstate.*
.terraform/
.terraform.lock.hcl  # 팀에서 공유해야 할 수도 있음
*.tfplan
crash.log
crash.*.log
override.tf
override.tf.json
*_override.tf
*_override.tf.json
*.tfvars
*.tfvars.json
```

## 10. 성능 최적화

### 10.1 병렬도 조정

```bash
# 기본: 10개 동시 실행
terraform apply

# 병렬도 증가 (대규모 인프라)
terraform apply -parallelism=20

# 병렬도 감소 (API 레이트 리밋 회피)
terraform apply -parallelism=5
```

### 10.2 프로바이더 캐싱

```hcl
# ~/.terraformrc
plugin_cache_dir = "$HOME/.terraform.d/plugin-cache"
```

```bash
# 또는 환경 변수
export TF_PLUGIN_CACHE_DIR="$HOME/.terraform.d/plugin-cache"
```

### 10.3 타겟팅

```bash
# 특정 리소스만 plan/apply
terraform plan -target=aws_instance.web
terraform plan -target=module.network

# 여러 타겟
terraform apply -target=aws_instance.web -target=aws_eip.web
```

### 10.4 Refresh 건너뛰기

```bash
# Refresh 건너뛰기 (대규모 State에서 속도 개선)
terraform plan -refresh=false

# State 정합성이 보장될 때만 사용
```

## 11. 모니터링 & 관측성

### 11.1 OpenTelemetry 지원

```bash
# OTLP gRPC 엔드포인트로 텔레메트리 전송
export OTEL_EXPORTER_OTLP_ENDPOINT="http://localhost:4317"
terraform plan
```

### 11.2 체크포인트

```bash
# HashiCorp 업데이트 체크 비활성화
export CHECKPOINT_DISABLE=1
```

### 11.3 Terraform 버전 관리

```hcl
# 버전 제약
terraform {
  required_version = ">= 1.5.0, < 2.0.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}
```

```bash
# tfenv로 버전 관리
tfenv install 1.9.0
tfenv use 1.9.0
```
