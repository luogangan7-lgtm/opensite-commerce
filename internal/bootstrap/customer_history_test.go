// Responsibility: customer-only history admission through production bootstrap.
// Dependencies: customer port fixture and real router; CI runs this test.
// Invariant: staff, guest and invalid customer cookies never reach order storage.
package bootstrap_test

import (
 "context"
 "encoding/base64"
 "net/http"
 "net/http/httptest"
 "testing"

 "github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
 "github.com/luogangan7-lgtm/opensite-commerce/internal/db"
 c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
 "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
)

func TestCustomerHistoryAdmission(t *testing.T) {
 const tenant="11111111-1111-4111-8111-111111111111"
 token:=base64.RawURLEncoding.EncodeToString(make([]byte,32))
 for _,tc:=range []struct{name,cookie string; failure error; want,calls int}{
  {name:"live",cookie:"__Host-osc_customer_session="+token,want:503,calls:1},
  {name:"logged out",want:401},
  {name:"staff cookie",cookie:"__Host-osc_admin_session="+token,want:401},
  {name:"forged",cookie:"__Host-osc_customer_session=forged",want:401},
  {name:"expired",cookie:"__Host-osc_customer_session="+token,failure:c.ErrUnauthorized,want:401},
  {name:"transient",cookie:"__Host-osc_customer_session="+token,failure:c.ErrUnavailable,want:503},
 } {t.Run(tc.name,func(t *testing.T){
  calls:=0
  customer:=wiringCustomer{token:token,failure:tc.failure,session:c.Session{TenantID:tenant,CustomerID:"22222222-2222-4222-8222-222222222222",ID:"33333333-3333-4333-8333-333333333333"}}
  server,err:=bootstrap.New(bootstrap.Config{Customer:customer,CustomerOrigin:"https://shop.example.test",CustomerSecret:make([]byte,32),Authenticator:rejectStaffAuth{t:t},Orders:orders.HTTPConfig{Read:func(context.Context,string,func(*db.Queries)error)error{calls++;return c.ErrUnavailable}}})
  if err!=nil{t.Fatal(err)}
  for _,suffix:=range []string{"","/99999999-9999-4999-8999-999999999999"}{
   before:=calls
   r:=httptest.NewRequest(http.MethodGet,"/api/v1/t/"+tenant+"/customer/orders"+suffix,nil)
   r.Header.Set("Cookie",tc.cookie);r.Header.Set("Authorization","Bearer staff-or-guest")
   w:=httptest.NewRecorder();server.Handler.ServeHTTP(w,r)
   if w.Code!=tc.want||calls-before!=tc.calls{t.Fatalf("HISTORY_ADMISSION_RED: status=%d want=%d storage=%d want=%d: %s",w.Code,tc.want,calls-before,tc.calls,w.Body.String())}
   if w.Header().Get("Cache-Control")!="no-store"{t.Fatal("history must be private no-store")}
  }
 })}
}
