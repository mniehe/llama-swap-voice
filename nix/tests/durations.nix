# Duration strings the module's converter must read exactly as Go's
# time.ParseDuration does. A leading sign is outside the module's syntax, so
# those rows must be refused whatever Go says.
[
  # The external review's table.
  "1000000000000000ns"
  "0.5ns0.5ns"
  "9223372036854775808ns"
  "2562048h"
  "0000000000000001s"
  "+1s"

  # The TimeoutStopSec cases in eval.nix.
  "30s"
  "60s"
  "10s"
  "5m"
  "1.25m"
  "1m30s"
  "1500000µs"
  "2h"
  ".5s"

  # The maximum, 9223372036854775807 ns, and just past it.
  "9223372036854775807ns"
  "2562047h"
  "2562047h47m16.854775807s"
  "2562047h47m16.854775808s"
  "9223372036854775.807us"
  "9223372036854775.808us"
  "9223372036.854775807s"
  "9223372036.854775808s"
  "153722867m16.854775807s"
  "1h2562047h"
  "12345678901234567890ns"
  "99999999999999999999h"

  # Syntax.
  "0"
  "0s"
  "-0"
  "+0"
  "-1s"
  ""
  "1"
  "90"
  "s"
  ".s"
  "1.s"
  "1x"
  "1sm"
  "1e3s"
  " 1s"
  "1s "
  "1h1h"
  "3h2m1.5s"
  "1us"
  "1µs"
  "1μs"
  "100.5s"
  "1.5h"

  # Fractions truncate per term to whole nanoseconds.
  "0.1ns"
  "0.9ns"
  "1.9ns"
  "0.0000000001h"
  "1.000000001s"
  "0.12345678901234567890123s"
  "0.99999999999999999999s"
  "1.1h"
  "0.3333333333h"
]
