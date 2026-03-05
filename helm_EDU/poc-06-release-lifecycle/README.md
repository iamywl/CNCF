# PoC-06: Helm v4 Release 라이프사이클

## 개요

Helm v4의 릴리스 상태 전이(Install/Upgrade/Rollback/Uninstall)와 리비전 관리를 시뮬레이션합니다.

## 시뮬레이션하는 패턴

| Action | 실제 소스 | 상태 전이 |
|--------|----------|----------|
| Install | `pkg/action/install.go` | pending-install → deployed/failed |
| Upgrade | `pkg/action/upgrade.go` | (현재→superseded) + pending-upgrade → deployed/failed |
| Rollback | `pkg/action/rollback.go` | (현재→superseded) + pending-rollback → deployed |
| Uninstall | `pkg/action/uninstall.go` | uninstalling → uninstalled/(삭제) |

## 실행 방법

```bash
go run main.go
```

## 상태 전이 다이어그램

```
Install:   (없음) ──→ pending-install ──→ deployed
                                        └→ failed

Upgrade:   deployed(현재) → superseded
           (새 리비전)     → pending-upgrade → deployed / failed

Rollback:  deployed(현재) → superseded
           (이전 복제)     → pending-rollback → deployed

Uninstall: deployed → uninstalling → uninstalled (keep-history)
                                    → (삭제됨)
```

## 핵심 개념

1. **리비전은 항상 증가**: 롤백도 새 리비전 번호(v4→v5)를 가짐
2. **deployed는 최대 1개**: 한 릴리스에서 deployed 상태는 항상 하나
3. **superseded**: 업그레이드/롤백 시 이전 릴리스의 상태
4. **롤백=복제**: 대상 리비전의 Chart/Config/Manifest를 새 리비전으로 복제
5. **KeepHistory**: uninstall 후에도 이력 조회 가능
