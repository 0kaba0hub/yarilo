package fail

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/dict"
)

func TestEveryOpReturnsFailDriver(t *testing.T) {
	d, err := New(dict.Config{})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer d.Close()

	ctx := context.Background()

	if _, _, err := d.Lookup(ctx, nil, "k"); !errors.Is(err, ErrFailDriver) {
		t.Errorf("Lookup err = %v, want ErrFailDriver", err)
	}
	if _, err := d.Iterate(ctx, nil, "", 0); !errors.Is(err, ErrFailDriver) {
		t.Errorf("Iterate err = %v, want ErrFailDriver", err)
	}
	if _, err := d.Begin(ctx, nil); !errors.Is(err, ErrFailDriver) {
		t.Errorf("Begin err = %v, want ErrFailDriver", err)
	}
	if err := d.ExpireScan(ctx); !errors.Is(err, ErrFailDriver) {
		t.Errorf("ExpireScan err = %v, want ErrFailDriver", err)
	}
}

func TestCustomMessage(t *testing.T) {
	d, _ := New(dict.Config{Settings: map[string]any{"message": "metadata feature disabled by admin"}})
	defer d.Close()
	_, _, err := d.Lookup(context.Background(), nil, "k")
	if err == nil || !strings.Contains(err.Error(), "metadata feature disabled by admin") {
		t.Errorf("custom message not surfaced; got %v", err)
	}
}

func TestRegisteredAtInit(t *testing.T) {
	for _, n := range dict.Drivers() {
		if n == "fail" {
			return
		}
	}
	t.Errorf("fail driver not registered: %v", dict.Drivers())
}
