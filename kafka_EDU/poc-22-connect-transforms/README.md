# PoC-22: Kafka Connect Transforms + File Connector 시뮬레이션

## 관련 문서
- [25-connect-transforms.md](../../kafka_EDU/25-connect-transforms.md)

## 시뮬레이션 내용
1. **SMT 체인**: InsertField → MaskField → ReplaceField → ValueToKey 순차 변환
2. **InsertField**: 정적 필드/타임스탬프 필드 추가
3. **ReplaceField**: 필드 이름 변경, 포함/제외 필터링
4. **MaskField**: 민감 필드 마스킹 (PII 보호)
5. **TimestampRouter**: 타임스탬프 기반 토픽 파티셔닝 (일별/주별)
6. **RegexRouter**: 정규식 기반 토픽 라우팅
7. **ValueToKey**: Value 필드에서 Key 추출
8. **FileStreamSource/Sink**: 파일 입출력 커넥터 파이프라인
9. **Dead Letter Queue**: 변환 실패 레코드 격리
10. **Converter (JSON)**: Connect 내부 ↔ 직렬화 포맷 변환

## 참조 소스
- `connect/transforms/src/main/java/.../InsertField.java`
- `connect/transforms/src/main/java/.../ReplaceField.java`
- `connect/transforms/src/main/java/.../MaskField.java`
- `connect/transforms/src/main/java/.../TimestampRouter.java`
- `connect/file/src/main/java/.../FileStreamSourceConnector.java`
- `connect/file/src/main/java/.../FileStreamSinkConnector.java`

## 실행
```bash
go run main.go
```
