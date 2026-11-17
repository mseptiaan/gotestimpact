package gitdiff

import (
	"reflect"
	"testing"
)

func TestParseUnifiedDiff(t *testing.T) {
	const out = `diff --git a/a/a.go b/a/a.go
index 1111111..2222222 100644
--- a/a/a.go
+++ b/a/a.go
@@ -3 +3 @@ func A() int {
-	return 1
+	return 2
@@ -10,2 +9,0 @@ func B() {
--- a line that looks like a header
-+++ another one
@@ -20,0 +19,3 @@
+func C() {}
+
+// added
diff --git a/new.go b/new.go
new file mode 100644
index 0000000..3333333
--- /dev/null
+++ b/new.go
@@ -0,0 +1,2 @@
+package m
+// x
diff --git a/old.go b/old.go
deleted file mode 100644
index 4444444..0000000
--- a/old.go
+++ /dev/null
@@ -1 +0,0 @@
-package m
\ No newline at end of file
diff --git "a/sp ace.go" "b/sp ace.go"
--- "a/sp ace.go"
+++ "b/sp ace.go"
@@ -1 +1 @@
-x
+y
`
	got, err := parseUnifiedDiff(out, "/r")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]*FileDiff{
		"/r/a/a.go":    {Path: "/r/a/a.go", Hunks: []Hunk{{3, 1}, {9, 0}, {19, 3}}},
		"/r/new.go":    {Path: "/r/new.go", Added: true, Hunks: []Hunk{{1, 2}}},
		"/r/old.go":    {Path: "/r/old.go", Deleted: true, Hunks: []Hunk{{0, 0}}},
		"/r/sp ace.go": {Path: "/r/sp ace.go", Hunks: []Hunk{{1, 1}}},
	}
	if !reflect.DeepEqual(got, want) {
		for k, v := range got {
			t.Logf("got %s: %+v", k, *v)
		}
		t.Fatal("mismatch")
	}
}
