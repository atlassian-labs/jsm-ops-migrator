package credentials

// FastPassphraseKDF makes argon2id cheap for the length of a test, so the
// suite stays fast. Wrapped keys record their parameters, so the provider
// under test still reads them back.
func FastPassphraseKDF() (restore func()) {
	t, m, p := argonTime, argonMemory, argonThreads
	argonTime, argonMemory, argonThreads = 1, 64, 1
	return func() { argonTime, argonMemory, argonThreads = t, m, p }
}
