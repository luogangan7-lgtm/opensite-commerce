// State validation is shared by order commands and their consumers; database
// transition functions independently enforce the same terminal boundaries.
package contract

func ValidateTransition(from, to State) error {
	switch from {
	case PendingPayment:
		if to == Paid || to == Cancelled {
			return nil
		}
	case Paid:
		if to == PartiallyFulfilled || to == Fulfilled || to == PartiallyRefunded || to == Refunded {
			return nil
		}
	case PartiallyFulfilled:
		if to == Fulfilled || to == PartiallyRefunded || to == Refunded {
			return nil
		}
	case Fulfilled:
		if to == PartiallyRefunded || to == Refunded {
			return nil
		}
	case PartiallyRefunded:
		if to == PartiallyRefunded || to == Refunded {
			return nil
		}
	}
	return ErrConflict
}
