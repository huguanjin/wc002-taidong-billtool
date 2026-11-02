package billing

import "testing"

func TestMergeGroupDiscountInputsKeepsExisting(t *testing.T) {
	existing := []GroupDiscountRow{
		{GroupKey: "A", Discount: 0.5, Note: "n"},
		{GroupKey: "B", Discount: 0.6},
	}
	d := 0.7
	got := MergeGroupDiscountInputs(existing, []GroupDiscountInput{
		{GroupKey: "B", Discount: &d},
		{GroupKey: "C", Discount: &d},
	})
	byKey := map[string]float64{}
	for _, it := range got {
		if it.Discount == nil {
			t.Fatalf("unexpected nil discount for %s", it.GroupKey)
		}
		byKey[it.GroupKey] = *it.Discount
	}
	if len(got) != 3 || byKey["A"] != 0.5 || byKey["B"] != 0.7 || byKey["C"] != 0.7 {
		t.Fatalf("unexpected merge result: %+v", byKey)
	}
}
