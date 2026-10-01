package tools

// The Phase 2 tools use explicit schemas rather than SDK reflection. This is
// deliberate: reflection emits open objects, unbounded strings, and array
// unions that Copilot Studio cannot consume safely.

func objectSchema(properties map[string]any, required []string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           properties,
		"required":             required,
	}
}

func optionalString(description string, max int) map[string]any {
	return map[string]any{"type": "string", "description": description, "maxLength": max}
}

func boundedInteger(minimum, maximum int) map[string]any {
	return map[string]any{"type": "integer", "minimum": minimum, "maximum": maximum}
}

func boundedEnum(values ...string) map[string]any {
	max := 1
	for _, value := range values {
		if len(value) > max {
			max = len(value)
		}
	}
	return map[string]any{"type": "string", "minLength": 1, "maxLength": max, "enum": values}
}

func boundedOutputStatus() map[string]any {
	return map[string]any{"type": "string", "minLength": 1, "maxLength": 64}
}

func commonStructuredOutputProperties() map[string]any {
	return map[string]any{
		"nl_audit_id": boundedSchema("audit ID", 128),
		"status":      boundedOutputStatus(),
		"error":       structuredErrorSchema(),
	}
}

func mergeProperties(base, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range extra {
		out[key] = value
	}
	return out
}

func listSpreadsheetsInputSchema() map[string]any {
	return objectSchema(map[string]any{}, []string{})
}

func listSpreadsheetsOutputSchema() map[string]any {
	sheet := objectSchema(map[string]any{
		"id":   boundedSchema("sheet ID", 128),
		"name": optionalString("sheet name", 512),
	}, []string{"id"})
	spreadsheet := objectSchema(map[string]any{
		"id":        boundedSchema("spreadsheet ID", 128),
		"name":      optionalString("spreadsheet name", 512),
		"region":    optionalString("Workiva region", 16),
		"synced_at": optionalString("last local sync time", 64),
		"sheets":    map[string]any{"type": "array", "maxItems": 1000, "items": sheet},
	}, []string{"id", "sheets"})
	return objectSchema(mergeProperties(commonStructuredOutputProperties(), map[string]any{
		"spreadsheets": map[string]any{"type": "array", "maxItems": 1000, "items": spreadsheet},
	}), []string{"spreadsheets"})
}

func readRangeInputSchema() map[string]any {
	return objectSchema(map[string]any{
		"spreadsheet_id": boundedSchema("spreadsheet ID", 128),
		"sheet_id":       boundedSchema("sheet ID", 128),
		"range":          boundedSchema("A1 cell range", 512),
	}, []string{"spreadsheet_id", "sheet_id", "range"})
}

func readRangeOutputSchema() map[string]any {
	rows := map[string]any{"type": "array", "maxItems": 1000, "items": map[string]any{"type": "array", "maxItems": 1000, "items": optionalString("cell value", 4096)}}
	return objectSchema(mergeProperties(commonStructuredOutputProperties(), map[string]any{
		"spreadsheet_id": boundedSchema("spreadsheet ID", 128),
		"sheet_id":       boundedSchema("sheet ID", 128),
		"range":          boundedSchema("A1 cell range", 512),
		"rows":           rows,
		"fetched_at":     boundedSchema("fetch time", 64),
	}), []string{"spreadsheet_id", "sheet_id", "range", "rows", "fetched_at"})
}

func searchFieldsInputSchema() map[string]any {
	return objectSchema(map[string]any{"query": boundedSchema("natural-language field query", 512)}, []string{"query"})
}

func searchFieldsOutputSchema() map[string]any {
	field := objectSchema(map[string]any{
		"name":           boundedSchema("mapped field name", 256),
		"spreadsheet_id": boundedSchema("spreadsheet ID", 128),
		"sheet_id":       boundedSchema("sheet ID", 128),
		"range":          boundedSchema("A1 cell range", 512),
		"field_type":     optionalString("mapped field type", 64),
		"description":    optionalString("field description", 2048),
		"cached_value":   optionalString("cached display value", 4096),
		"cached_at":      optionalString("cache time", 64),
	}, []string{"name", "spreadsheet_id", "sheet_id", "range", "field_type"})
	return objectSchema(mergeProperties(commonStructuredOutputProperties(), map[string]any{
		"fields": map[string]any{"type": "array", "maxItems": 1000, "items": field},
	}), []string{"fields"})
}

func getFieldInputSchema() map[string]any {
	return objectSchema(map[string]any{"name": boundedSchema("exact mapped field name", 256)}, []string{"name"})
}

func getFieldOutputSchema() map[string]any {
	return objectSchema(mergeProperties(commonStructuredOutputProperties(), map[string]any{
		"name":           boundedSchema("mapped field name", 256),
		"spreadsheet_id": boundedSchema("spreadsheet ID", 128),
		"sheet_id":       boundedSchema("sheet ID", 128),
		"range":          boundedSchema("A1 cell range", 512),
		"field_type":     optionalString("mapped field type", 64),
		"description":    optionalString("field description", 2048),
		"aliases":        optionalString("field aliases", 2048),
		"value":          optionalString("display value", 4096),
		"fetched_at":     boundedSchema("fetch time", 64),
		"source":         boundedSchema("cache or live source", 16),
	}), []string{"name", "spreadsheet_id", "sheet_id", "range", "field_type", "value", "fetched_at", "source"})
}

func updateFieldInputSchema() map[string]any {
	return objectSchema(map[string]any{
		"name":          boundedSchema("exact mapped field name", 256),
		"value":         optionalString("new field value", 4096),
		"confirm_token": optionalString("single-use confirmation token", 4096),
	}, []string{"name", "value"})
}

func updateFieldOutputSchema() map[string]any {
	return objectSchema(mergeProperties(commonStructuredOutputProperties(), map[string]any{
		"confirm_token":  optionalString("single-use confirmation token", 4096),
		"field":          boundedSchema("mapped field name", 256),
		"spreadsheet_id": boundedSchema("spreadsheet ID", 128),
		"sheet_id":       boundedSchema("sheet ID", 128),
		"range":          boundedSchema("A1 cell range", 512),
		"before":         optionalString("previous value", 4096),
		"after_preview":  optionalString("proposed value", 4096),
		"after":          optionalString("written value", 4096),
		"workiva_op_url": optionalString("Workiva operation reference", 1024),
		"message":        optionalString("bounded operator message", 2048),
	}), []string{"status", "field", "spreadsheet_id", "sheet_id", "range", "before"})
}

func syncMappingInputSchema() map[string]any {
	return objectSchema(map[string]any{
		"spreadsheet_id": boundedSchema("spreadsheet ID", 128),
		"sheet_id":       boundedSchema("sheet ID", 128),
		"name_column":    optionalString("name column", 16),
		"value_column":   optionalString("value column", 16),
		"start_row":      boundedInteger(0, 1000000),
	}, []string{"spreadsheet_id", "sheet_id"})
}

func syncMappingOutputSchema() map[string]any {
	return objectSchema(mergeProperties(commonStructuredOutputProperties(), map[string]any{
		"spreadsheet_id": boundedSchema("spreadsheet ID", 128),
		"sheet_id":       boundedSchema("sheet ID", 128),
		"fields_count":   boundedInteger(0, 1000),
		"fields":         map[string]any{"type": "array", "maxItems": 1000, "items": boundedSchema("mapped field name", 256)},
		"message":        optionalString("bounded sync message", 2048),
	}), []string{"status", "spreadsheet_id", "sheet_id", "fields_count", "fields"})
}

func auditEntrySchema() map[string]any {
	return objectSchema(map[string]any{
		"seq":            boundedInteger(0, 2147483647),
		"ts":             boundedSchema("audit timestamp", 64),
		"tenant_id":      boundedSchema("tenant identifier", 128),
		"actor":          optionalString("audit actor", 256),
		"tool":           boundedSchema("tool name", 128),
		"action":         boundedSchema("audit action", 128),
		"target":         optionalString("audit target", 1024),
		"before_json":    optionalString("redacted before payload", 4096),
		"after_json":     optionalString("redacted after payload", 4096),
		"workiva_op_url": optionalString("Workiva operation reference", 1024),
		"audit_id":       optionalString("audit correlation ID", 128),
		"prev_hash":      boundedSchema("previous audit hash", 128),
		"hash":           boundedSchema("audit hash", 128),
		"hash_version":   boundedInteger(1, 2),
	}, []string{"seq", "ts", "tenant_id", "tool", "action", "prev_hash", "hash", "hash_version"})
}

func auditTrailInputSchema() map[string]any {
	return objectSchema(map[string]any{
		"limit":  boundedInteger(0, 1000),
		"target": optionalString("exact audit target", 1024),
	}, []string{})
}

func auditTrailOutputSchema() map[string]any {
	return objectSchema(mergeProperties(commonStructuredOutputProperties(), map[string]any{
		"count":   boundedInteger(0, 100),
		"entries": map[string]any{"type": "array", "maxItems": 100, "items": auditEntrySchema()},
	}), []string{"count", "entries"})
}
