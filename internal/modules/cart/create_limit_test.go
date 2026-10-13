// Cart limiter tests use a fixed clock and public authentication seam; bootstrap
// consumes this seam. No sleeps or credentials enter quota keys; denial is atomic.
package cart

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func TestAnonymousCreateBuckets(t *testing.T) {
	at := time.Unix(1700000000, 0)
	l := newCreateLimiter(AnonymousCreateLimit{IPBurst: 2, TokenBurst: 1, RefillInterval: time.Minute, MaxKeys: 4})
	l.now = func() time.Time { return at }
	token := []byte("stored hash")
	if !l.allow("192.0.2.1:1", token) || l.allow("192.0.2.2:1", token) {
		t.Fatal("token quota reset on IP change")
	}
	if !l.allow("[::ffff:192.0.2.1]:2", nil) || l.allow("192.0.2.1:3", nil) {
		t.Fatal("IP or port normalization bypass")
	}
	at = at.Add(time.Minute)
	if !l.allow("192.0.2.1:1", token) {
		t.Fatal("bucket did not refill")
	}
	if !l.allow("192.0.2.3:1", nil) || !l.allow("192.0.2.4:1", nil) || l.allow("192.0.2.5:1", nil) {
		t.Fatal("unbounded map or premature eviction")
	}
	at = at.Add(2 * time.Minute)
	if !l.allow("192.0.2.5:1", nil) || len(l.buckets) > 4 {
		t.Fatal("full idle buckets not safely reclaimed")
	}
}
func TestAnonymousCreateConcurrentQuota(t *testing.T) {
	l := newCreateLimiter(AnonymousCreateLimit{IPBurst: 2, RefillInterval: time.Hour})
	l.now = func() time.Time { return time.Unix(1700000000, 0) }
	var wg sync.WaitGroup
	results := make(chan bool, 100)
	for i := 0; i < 100; i++ {
		wg.Go(func() { results <- l.allow("192.0.2.1:1234", nil) })
	}
	wg.Wait()
	close(results)
	admitted := 0
	for ok := range results {
		if ok {
			admitted++
		}
	}
	if admitted != 2 {
		t.Fatalf("concurrent admission=%d want2", admitted)
	}
}
func TestAnonymousCreateTokenAndCustomerAdmission(t *testing.T) {
	const tenant = "10000000-0000-4000-8000-000000000001"
	const token = "YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWE"
	m := NewPostgres(Config{AnonymousCreateLimit: AnonymousCreateLimit{IPBurst: 5, TokenBurst: 1}}, nil)
	request := func(ip, operation string) error {
		r := httptest.NewRequest("POST", "/", nil)
		r.RemoteAddr = ip
		r.SetPathValue("tenant_id", tenant)
		r.Header.Set("X-Cart-Token", token)
		_, err := m.Authenticate(r.Context(), r, platform.RouteSpec{Access: platform.Storefront, OperationID: operation})
		return err
	}
	if err := request("192.0.2.1:1", "createCart"); err != nil {
		t.Fatal(err)
	}
	var failure *apperrors.Error
	if err := request("192.0.2.2:1", "createCart"); !errors.As(err, &failure) || failure.Status != 429 {
		t.Fatalf("token limit %v", err)
	}
	if err := request("192.0.2.2:1", "getCart"); err != nil {
		t.Fatalf("creation quota affected read %v", err)
	}
	m = NewPostgres(Config{AnonymousCreateLimit: AnonymousCreateLimit{IPBurst: 1}}, func(context.Context, *http.Request) (contract.AuthorizedActor, error) {
		return contract.AuthorizedActor{TenantID: tenant, CustomerID: "20000000-0000-4000-8000-000000000001"}, nil
	})
	for i := 0; i < 3; i++ {
		r := httptest.NewRequest("POST", "/", nil)
		r.SetPathValue("tenant_id", tenant)
		r.RemoteAddr = fmt.Sprint("192.0.2.1:", i)
		if _, err := m.Authenticate(r.Context(), r, platform.RouteSpec{Access: platform.Storefront, OperationID: "createCart"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAnonymousCreateIPCannotRotateCapabilityOrIPv6Address(t *testing.T) {
	l := newCreateLimiter(AnonymousCreateLimit{IPBurst: 1, TokenBurst: 10})
	l.now = func() time.Time { return time.Unix(1700000000, 0) }
	if !l.allow("192.0.2.1:1", []byte("first")) || l.allow("192.0.2.1:2", []byte("second")) {
		t.Fatal("rotating capability bypassed IP")
	}
	if !l.allow("[2001:db8:1:2::1]:1", nil) || l.allow("[2001:db8:1:2::2]:2", nil) {
		t.Fatal("rotating IPv6 host bypassed /64 quota")
	}
	if !l.allow("[2001:db8:1:3::1]:1", nil) {
		t.Fatal("independent /64 denied")
	}
}
