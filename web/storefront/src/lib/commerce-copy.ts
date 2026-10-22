/** Responsibility: typed purchasing interface copy; dependencies: none; callers: purchase pages/islands.
 * Invariants: fixture claims are explicit; unavailable production actions never promise fulfillment or real authentication.
 */
const english = {
  cart: 'Cart', account: 'Account', checkout: 'Checkout', orderConfirmation: 'Order confirmation',
  purchaseDescription: 'Review your items and checkout details.', accountDescription: 'Manage your customer session.',
  fixtureNotice: 'Contract fixture — test data only. No real purchase, payment, or customer account is created.',
  temporarilyUnavailable: 'Temporarily unavailable', unavailableDescription: 'Purchasing is temporarily unavailable. You can continue browsing products.',
  browseProducts: 'Browse products', variant: 'Variant', quantity: 'Quantity', addCart: 'Add to cart', viewCart: 'View cart',
  added: 'Item added to cart.', outOfStock: 'Unavailable', selectedPrice: 'Selected price',
  loading: 'Loading…', retry: 'Try again', requestFailed: 'We could not complete this request. Try again.',
  priceChanged: 'Prices have changed. Review the updated server totals before confirming payment.',
  acceptUpdatedTotal: 'Confirm updated total',
  cartChanged: 'Your cart changed in another tab. We refreshed it and kept those changes. Review the updated quantities before trying again.',
  emptyCart: 'Your cart is empty', emptyCartDescription: 'Choose a product to start your cart.',
  updateQuantity: 'Update quantity', remove: 'Remove', subtotal: 'Subtotal', tax: 'Tax', shipping: 'Shipping', total: 'Total',
  pricingUnavailable: 'Pricing unavailable', lineUnavailable: 'This item is unavailable. Remove it before checkout.',
  continueCheckout: 'Continue to checkout', backCart: 'Back to cart', checkoutContact: 'Contact details', shippingAddress: 'Shipping address',
  email: 'Email', name: 'Full name', phone: 'Phone', country: 'Country code', address1: 'Address line 1', address2: 'Address line 2',
  city: 'City', subdivision: 'Region', postal_code: 'Postal code', optional: 'Optional', countryHelp: 'Use two uppercase letters, for example US.',
  required: 'This field is required.', tooLong: 'This value is too long.', invalidCountry: 'Enter a two-letter uppercase country code.', invalidEmail: 'Enter a valid email address.',
  reviewTotals: 'Review totals', totalsDescription: 'Tax and shipping are returned by the checkout service for this address.', shippingMethods: 'Available shipping methods',
  startCheckout: 'Start test checkout', paymentPending: 'Payment confirmation pending',
  paymentPendingDescription: 'This test checkout is ready. An authorization is not a completed payment. Confirm a fake payment outcome below.',
  fakeCapture: 'Fake payment: capture', fakeFail: 'Fake payment: fail', fakeCancel: 'Fake payment: cancel',
  paymentCaptured: 'Test payment captured', paymentFailed: 'Test payment failed', paymentCanceled: 'Test payment canceled',
  paymentRetry: 'The test order is not complete. Return to the cart to review your items.',
  viewConfirmation: 'View order confirmation', noCompletion: 'No completed order', noCompletionDescription: 'An order confirmation is available only after a captured payment.',
  orderId: 'Order reference', paymentStatus: 'Payment status', returnProducts: 'Continue browsing',
  signIn: 'Sign in', register: 'Register', password: 'Password', signOut: 'Sign out', signedIn: 'Signed in to the test customer session.',
  guestSession: 'You are browsing as a guest.', accountHelp: 'Use this contract fixture to test customer checkout. Real customer authentication is not connected.',
  registered: 'Test registration received. Email verification is pending; this does not sign you in.',
  passwordHelp: 'Use 8–128 characters.', invalidPassword: 'Use a password of 8–128 characters.', signedOut: 'Signed out of the test customer session.',
} as const;
export type CommerceCopyKey = keyof typeof english;
export type CommerceDictionary = Record<CommerceCopyKey, string>;
const dictionaries: Record<'en', CommerceDictionary> = { en: english };
export function commerceCopy(_locale = 'en', fixture = true): CommerceDictionary { return fixture ? dictionaries.en : {...dictionaries.en,
  signedIn:'You are signed in.',signedOut:'You are signed out.',
  accountHelp:'Sign in to shop with your account, or register and verify your email address.',
  registered:'If the address is eligible, a verification email has been sent. Verify your email before signing in.',
  passwordHelp:'Registration requires 12–1024 UTF-8 bytes. Sign in with your existing password.',
  invalidPassword:'Enter your password. New passwords must contain 12–1024 UTF-8 bytes.',
}; }
