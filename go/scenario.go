// The token owner's identity, re-derived from the contest's single fixed seed.
//
// The suite addresses the viewer by login, but the bearer token itself says nothing
// about which login it names. Every user, repository and path the suite uses comes
// from SCENARIO_SEED through a pure generator, so replaying that generator lands on
// the same viewer in every trial, live or replayed.
//
// Only the user stream is needed here: users[0] is the viewer. The constants and
// the arithmetic below are the scenario's own; the layout is this port's.

package main

import "sync"

const scenarioSeed uint64 = 0x6a09e667f3bcc909

var givenNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars",
	"mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}

var familyNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller",
	"lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber",
	"xu", "yilmaz", "zimmer",
}

// ScenarioUser is one generated user: id, login, display name, email.
type ScenarioUser struct {
	ID    int
	Login string
	Name  string
	Email string
}

// fnvHash is FNV-1a over 64 bits, the scenario's string hasher. Go's uint64
// arithmetic wraps modulo 2^64, which is exactly the masking the original does.
func fnvHash(text string) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		hash ^= uint64(text[i])
		hash *= 0x100000001b3
	}
	return hash
}

// scramble is SplitMix64's finalizer: it spreads a seed over 64 bits.
func scramble(value uint64) uint64 {
	z := value + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// xorShift is xorshift64*, the scenario's generator. A zero seed falls back to
// the scenario seed, as in the original.
type xorShift struct {
	state uint64
}

func newXorShift(seed uint64) *xorShift {
	if seed == 0 {
		seed = scenarioSeed
	}
	return &xorShift{state: seed}
}

func (x *xorShift) nextU64() uint64 {
	v := x.state
	v ^= v >> 12
	v ^= v << 25
	v ^= v >> 27
	x.state = v
	return v * 0x2545f4914f6cdd1d
}

func (x *xorShift) nextU32() uint32 {
	return uint32(x.nextU64() >> 32)
}

// nextInt is a uniform integer in [0, max), rejection-sampled so the top of the
// range is not biased.
func (x *xorShift) nextInt(max int) int {
	if max == 1 {
		return 0
	}
	const range32 = uint64(4294967296)
	limit := range32 - (range32 % uint64(max))
	for {
		if v := uint64(x.nextU32()); v < limit {
			return int(v % uint64(max))
		}
	}
}

func (x *xorShift) pick(items []string) string {
	return items[x.nextInt(len(items))]
}

// streamRng is the generator for one named part of the scenario.
func streamRng(label string) *xorShift {
	return newXorShift(scramble(scenarioSeed ^ fnvHash(label)))
}

// makeUsers builds count users with unique logins; a later collision earns a
// -2, -3, ... suffix.
func makeUsers(rng *xorShift, count int) []ScenarioUser {
	taken := make(map[string]bool, count)
	users := make([]ScenarioUser, 0, count)
	for i := 0; i < count; i++ {
		given := rng.pick(givenNames)
		family := rng.pick(familyNames)
		base := sanitizeLogin(given + family)
		login := base
		for suffix := 2; taken[login]; suffix++ {
			login = base + "-" + itoa(suffix)
		}
		taken[login] = true
		users = append(users, ScenarioUser{
			ID:    1000 + i,
			Login: login,
			Name:  given + " " + family,
			Email: login + "@example.invalid",
		})
	}
	return users
}

// sanitizeLogin keeps only the characters a login may hold.
func sanitizeLogin(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			out = append(out, c)
		}
	}
	return string(out)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

var (
	usersOnce sync.Once
	usersMemo []ScenarioUser
)

func allUsers() []ScenarioUser {
	usersOnce.Do(func() {
		usersMemo = makeUsers(streamRng("users"), 12)
	})
	return usersMemo
}

// viewerLogin is the login the bearer token names: the scenario's first user.
func viewerLogin() string {
	if users := allUsers(); len(users) > 0 {
		return users[0].Login
	}
	return "viewer"
}

// viewerUser is the token owner's user record.
func viewerUser() ScenarioUser {
	return allUsers()[0]
}
