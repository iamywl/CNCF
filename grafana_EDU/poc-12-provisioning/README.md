# PoC 12: 프로비저닝 시스템

## 개요

Grafana의 파일 기반 프로비저닝 시스템을 시뮬레이션한다.
Grafana는 YAML 설정 파일을 통해 데이터소스, 대시보드, 알림 규칙 등을
코드로 관리할 수 있는 프로비저닝 기능을 제공한다.
이를 통해 Infrastructure as Code 방식의 Grafana 관리가 가능하다.

## Grafana 실제 구조

프로비저닝은 `pkg/services/provisioning/` 디렉토리에 구현되어 있다.

핵심 파일:
- `pkg/services/provisioning/provisioning.go` - ProvisioningService
- `pkg/services/provisioning/datasources/` - 데이터소스 프로비저닝
- `pkg/services/provisioning/dashboards/` - 대시보드 프로비저닝
- `pkg/services/provisioning/alerting/` - 알림 프로비저닝

## 프로비저닝 순서

```
시작 → 데이터소스 프로비저닝 → 플러그인 프로비저닝 → 알림 프로비저닝 → 대시보드 프로비저닝
```

데이터소스가 먼저 프로비저닝되어야 대시보드에서 참조할 수 있다.

## 시뮬레이션 내용

1. **ProvisioningService**: 전체 프로비저닝 오케스트레이션
2. **설정 파일 파싱**: 간단한 YAML-유사 포맷 (키: 값)
3. **데이터소스 프로비저닝**: 이름, 타입, URL, 접근 방식
4. **대시보드 프로비저닝**: 폴더, 경로, 옵션
5. **파일 감시 시뮬레이션**: 디렉토리 폴링으로 변경 감지
6. **변경 적용**: 생성/수정/삭제 처리
7. **버전 충돌 핸들링**: 프로비저닝 vs 수동 변경

## 실행

```bash
go run main.go
```

## 학습 포인트

- Infrastructure as Code 패턴 (선언적 설정)
- 파일 기반 프로비저닝의 장점과 한계
- 리소스 간 의존성 순서 (데이터소스 → 대시보드)
- 파일 감시를 통한 자동 변경 감지
- 프로비저닝된 리소스 vs 수동 관리 리소스의 충돌 처리
