// Customer transport tests run in GitHub CI, including every cookie and CSRF gate.
// Dependencies: public customer port and httptest; no employee or database authority.
package customerhttp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

const tenant = "11111111-1111-4111-8111-111111111111"
const origin = "https://shop.example.test"
const sessionPath = "/api/v1/t/"+tenant+"/customer/session"
type fake struct { calls int; issued string; limited bool }
func(f *fake) Register(context.Context,string,string,string,netip.Addr)error{f.calls++;return nil}
func(f *fake) Verify(context.Context,string,string)error{f.calls++;return nil}
func(f *fake) RequestReset(context.Context,string,string,netip.Addr)error{f.calls++;return nil}
func(f *fake) Reset(context.Context,string,string,string,netip.Addr)error{f.calls++;return nil}
func(f *fake) Login(context.Context,string,string,string,netip.Addr,string)(c.Session,error){f.calls++;if f.limited{return c.Session{},&c.RateLimitError{RetryAfter:7*time.Second}};return c.Session{ID:tenant,TenantID:tenant,CustomerID:tenant,Email:"buyer@example.test",Token:f.issued,ExpiresAt:time.Now().Add(time.Hour),IdleExpiresAt:time.Now().Add(time.Hour)},nil}
func(f *fake) Current(_ context.Context,t,token string)(c.Session,error){if token!=f.issued||t!=tenant{return c.Session{},c.ErrUnauthorized};return c.Session{ID:tenant,TenantID:tenant,CustomerID:tenant,Email:"buyer@example.test",ExpiresAt:time.Now().Add(time.Hour),IdleExpiresAt:time.Now().Add(time.Hour)},nil}
func(f *fake) Logout(context.Context,string,string)error{f.calls++;return nil}
func randomToken(t *testing.T)string{t.Helper();b:=make([]byte,32);if _,e:=rand.Read(b);e!=nil{t.Fatal(e)};return base64.RawURLEncoding.EncodeToString(b)}
func setup(t *testing.T)(*httpapi.Router,*fake){t.Helper();key:=make([]byte,32);if _,e:=rand.Read(key);e!=nil{t.Fatal(e)};f:=&fake{issued:randomToken(t)};router:=httpapi.New(nil,nil);for _,route:=range New(Config{Service:f,Origin:origin,Secret:key}).Routes(){if e:=router.Register(route.Spec,route.Handler);e!=nil{t.Fatal(e)}};return router,f}
func request(router http.Handler,method,path,body string,headers map[string]string,cookies ...*http.Cookie)*httptest.ResponseRecorder{r:=httptest.NewRequest(method,origin+path,strings.NewReader(body));r.RemoteAddr="192.0.2.1:2345";if body!=""{r.Header.Set("Content-Type","application/json")};for k,v:=range headers{r.Header.Set(k,v)};for _,c:=range cookies{r.AddCookie(c)};w:=httptest.NewRecorder();router.ServeHTTP(w,r);return w}
func challenge(t *testing.T,router http.Handler)(string,*http.Cookie){t.Helper();w:=request(router,"GET",sessionPath,"",nil);if w.Code!=200{t.Fatalf("CUSTOMER_AUTH_HTTP_RED: challenge status %d",w.Code)};var b struct{CSRF string `json:"csrf_token"`};if e:=json.Unmarshal(w.Body.Bytes(),&b);e!=nil||b.CSRF==""{t.Fatal("challenge absent")};for _,c:=range w.Result().Cookies(){if c.Name==ChallengeCookie{return b.CSRF,c}};t.Fatal("challenge cookie absent");return "",nil}
func TestCustomerOriginAndSignedCSRF(t *testing.T){
	r,f:=setup(t);csrf,cookie:=challenge(t,r)
	for _,test:=range []struct{name,source,token string; cookie bool}{{"missing","","",false},{"foreign","https://evil.example.test",csrf,true},{"wrong",origin,"unsigned",true},{"no-cookie",origin,csrf,false}}{t.Run(test.name,func(t *testing.T){var cookies []*http.Cookie;if test.cookie{cookies=append(cookies,cookie)};w:=request(r,"POST",sessionPath,`{"email":"buyer@example.test","password":"ignored"}`,map[string]string{"Origin":test.source,"X-CSRF-Token":test.token},cookies...);if w.Code!=403{t.Errorf("CSRF admission status %d",w.Code)}})}
	if f.calls!=0{t.Fatal("CSRF rejection reached domain")}
}
func TestCustomerCookieFlagsRotationAndNoBearerJSON(t *testing.T){
	r,f:=setup(t);csrf,cookie:=challenge(t,r);credential:=randomToken(t);body,_:=json.Marshal(map[string]string{"email":"buyer@example.test","password":credential})
	w:=request(r,"POST",sessionPath,string(body),map[string]string{"Origin":origin,"X-CSRF-Token":csrf},cookie)
	if w.Code!=200{t.Fatalf("login status %d",w.Code)};if strings.Contains(w.Body.String(),f.issued){t.Fatal("session bearer leaked JSON")}
	var found bool;for _,c:=range w.Result().Cookies(){if c.Name==SessionCookie{found=true;if !c.Secure||!c.HttpOnly||c.SameSite!=http.SameSiteLaxMode||c.Path!="/"||c.Domain!=""||c.Value!=f.issued{t.Fatal("customer cookie policy")}}};if !found{t.Fatal("session cookie absent")}
}
func TestCustomerStrictFieldsAndRetryAfter(t *testing.T){
	r,f:=setup(t);csrf,cookie:=challenge(t,r);headers:=map[string]string{"Origin":origin,"X-CSRF-Token":csrf}
	for _,body:=range []string{`{"email":"a@example.test","email":"b@example.test","password":"ignored"}`,`{"Email":"a@example.test","password":"ignored"}`,`{"email":"a@example.test","password":"ignored","customer_id":"forged"}`}{w:=request(r,"POST",sessionPath,body,headers,cookie);if w.Code!=400{t.Errorf("strict body status %d",w.Code)}}
	if f.calls!=0{t.Fatal("invalid JSON reached domain")};f.limited=true;credential:=randomToken(t);body,_:=json.Marshal(map[string]string{"email":"buyer@example.test","password":credential});w:=request(r,"POST",sessionPath,string(body),headers,cookie);if w.Code!=429||w.Header().Get("Retry-After")!="7"{t.Fatal("rate response lost")}
}
func TestCustomerCookieAmbiguityAndTenantIsolation(t *testing.T){
	r,f:=setup(t);cookie:=&http.Cookie{Name:SessionCookie,Value:f.issued};w:=request(r,"GET",sessionPath,"",nil,cookie,cookie);if w.Code!=401{t.Fatalf("duplicate cookie status %d",w.Code)}
	w=request(r,"GET",strings.Replace(sessionPath,tenant,"22222222-2222-4222-8222-222222222222",1),"",nil,cookie);if w.Code!=401{t.Fatalf("cross tenant session status %d",w.Code)}
}
