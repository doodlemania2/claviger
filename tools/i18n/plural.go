package i18n

// PluralCategory returns the CLDR plural category ("zero", "one", "two",
// "few", "many" or "other") of the integer n in the language. Languages
// without plural forms (Vietnamese, Korean, Chinese, ...) always give "other".
// The rules are the CLDR cardinal rules for integers.
func PluralCategory(tag string, n int) string {
	if n < 0 {
		n = -n
	}

	switch Primary(tag) {
	case "ar":
		switch {
		case n == 0:
			return "zero"
		case n == 1:
			return "one"
		case n == 2:
			return "two"
		case n%100 >= 3 && n%100 <= 10:
			return "few"
		case n%100 >= 11:
			return "many"
		}

		return "other"
	case "fr", "pt":
		if n == 0 || n == 1 {
			return "one"
		}

		if n != 0 && n%1000000 == 0 {
			return "many"
		}

		return "other"
	case "es":
		if n == 1 {
			return "one"
		}

		if n != 0 && n%1000000 == 0 {
			return "many"
		}

		return "other"
	case "tl", "fil":
		if n >= 1 && n <= 3 {
			return "one"
		}

		if d := n % 10; d != 4 && d != 6 && d != 9 {
			return "one"
		}

		return "other"
	case "vi", "ko", "zh", "ja", "th", "id", "ms":
		return "other"
	default: // English and most European languages
		if n == 1 {
			return "one"
		}

		return "other"
	}
}
