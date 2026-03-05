# PoC 02: VMConfig, VMDirectory, OCIManifest 데이터 모델 시뮬레이션

## 개요

tart의 핵심 데이터 모델인 VMConfig, VMDirectory, OCI Manifest 구조체를 Go로 시뮬레이션한다.
VMConfig는 VM의 설정을 JSON으로 직렬화/역직렬화하며, CPU/메모리 유효성 검증을 포함한다.
VMDirectory는 baseURL을 기준으로 config.json, disk.img, nvram.bin 등의 파일 경로를 관리한다.
OCIManifest는 OCI 이미지 스펙에 따라 config, layers, annotations를 포함한다.

## 실행 방법

```bash
go run main.go
```

## 핵심 시뮬레이션 포인트

| 구성 요소 | 시뮬레이션 내용 | 실제 tart 동작 |
|-----------|----------------|---------------|
| VMConfig | JSON 직렬화/역직렬화, CPU/메모리 검증 | struct VMConfig: Codable, setCPU/setMemory |
| VMDirectory | baseURL 기반 파일 경로 파생, 상태 판별 | struct VMDirectory: Prunable |
| VMDisplayConfig | width/height/unit 문자열 표현 | struct VMDisplayConfig: Codable, Equatable |
| OCIManifest | schemaVersion=2, config, layers, annotations | struct OCIManifest: Codable, Equatable |
| OCIManifestLayer | mediaType, digest, uncompressedSize 어노테이션 | struct OCIManifestLayer: Hashable |
| VMState | Running/Suspended/Stopped 판별 | enum State (PIDLock + state.vzvmsave) |
| MACAddress | locally administered MAC 주소 생성 | VZMACAddress.randomLocallyAdministered() |

## tart 실제 소스코드 참조 경로

- `Sources/tart/VMConfig.swift` — VMConfig 구조체 정의, JSON 인코딩/디코딩, CPU/메모리 유효성 검증
- `Sources/tart/VMDirectory.swift` — VMDirectory 구조체, State enum, 파일 경로 파생, initialized 검사
- `Sources/tart/OCI/Manifest.swift` — OCIManifest, OCIManifestLayer, OCIManifestConfig, 미디어 타입 상수
- `Sources/tart/Platform/OS.swift` — enum OS (darwin, linux)
- `Sources/tart/Platform/Architecture.swift` — enum Architecture (arm64, amd64)
- `Sources/tart/DiskImageFormat.swift` — enum DiskImageFormat (raw, asif)
