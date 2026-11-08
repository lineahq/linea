package ottlreplay

import "reflect"

// flattensLogs reports whether a transform processor configuration flattens
// logs before its statements run. The setting moved between releases:
// flatten_data (Config.FlattenData) up to 0.164, flatten.log
// (Config.Flatten.Log) from 0.164, and 0.165 removed flatten_data. The
// fields are read by name so the replay compiles against every release of
// the support table without an OTTL variant; a field a release lacks reads
// as false.
func flattensLogs(cfg any) bool {
	v := reflect.ValueOf(cfg)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return false
	}
	if boolField(v, "FlattenData") {
		return true
	}
	if f := v.FieldByName("Flatten"); f.IsValid() && f.Kind() == reflect.Struct {
		return boolField(f, "Log")
	}
	return false
}

func boolField(v reflect.Value, name string) bool {
	f := v.FieldByName(name)
	return f.IsValid() && f.Kind() == reflect.Bool && f.Bool()
}
