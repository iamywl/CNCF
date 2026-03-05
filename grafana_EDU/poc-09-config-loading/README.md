# PoC 09: 설정 로딩

## 개요

Grafana의 설정 로딩 시스템을 시뮬레이션한다.
Grafana는 INI 파일, 환경변수, CLI 인자의 3계층 설정 구조를 사용하며,
우선순위가 높은 설정이 낮은 설정을 덮어쓴다.

## Grafana 실제 구조

설정 로딩은 `pkg/setting/` 디렉토리에 구현되어 있다.

핵심 파일:
- `pkg/setting/setting.go` - Cfg 구조체, 설정 로딩 메인 로직
- `pkg/setting/setting_unified_alerting.go` - 알림 관련 설정
- `conf/defaults.ini` - 기본 설정값
- `conf/sample.ini` - 사용자 설정 예시

## 설정 우선순위

```
낮음 ←──────────────────────────────── 높음
defaults.ini → custom.ini → 환경변수 → CLI 인자
```

환경변수 매핑 규칙:
- 접두사: `GF_`
- 섹션: 대문자 (e.g., `[server]` → `SERVER`)
- 키: 대문자, 점(.)을 밑줄(_)로 변환
- 예: `[server]` 섹션의 `http_port` → `GF_SERVER_HTTP_PORT`

## 시뮬레이션 내용

1. **INI 파서**: 섹션, 키=값, 주석 처리
2. **4계층 설정 로딩**: defaults → INI → 환경변수 → CLI
3. **환경변수 매핑**: GF_SECTION_KEY 패턴
4. **출처 추적**: 각 설정값이 어느 계층에서 결정되었는지 표시
5. **최종 병합 설정**: 모든 계층을 병합한 결과 출력

## 실행

```bash
go run main.go
```

## 학습 포인트

- 다계층 설정 관리 패턴
- INI 파일 포맷 파싱 기법
- 환경변수를 통한 설정 오버라이드 (컨테이너 환경에서 유용)
- 설정 출처 추적을 통한 디버깅 용이성
