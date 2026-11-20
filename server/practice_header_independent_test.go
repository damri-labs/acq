package main
import("net/http/httptest";"net/http";"encoding/json";"strings";"testing")
func TestIndependentPracticeJoinCommandHeaderSafety(t *testing.T){
 t.Setenv("ACQ_PRACTICE_PEER","1");t.Setenv("ACQ_PUBLIC_BASE_URL","")
 srv:=newTestServer(t);defer srv.Close()
 req:=httptest.NewRequest(http.MethodPost,"http://localhost/practice",nil)
 req.Header.Set("X-Forwarded-Proto","https;printf INERT_REVIEW_MARKER;")
 w:=httptest.NewRecorder();handlePractice(w,req)
 var r map[string]any;json.Unmarshal(w.Body.Bytes(),&r)
 j,_:=r["join"].(string)
 if strings.Contains(j,";printf INERT_REVIEW_MARKER;"){t.Fatal("untrusted forwarded scheme became shell operators in copyable join command; command not executed")}
}
