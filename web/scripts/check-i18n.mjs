// The portal's catalogue check (brief WP-17), run in CI before the
// build (`pnpm check:i18n`); it fails, naming each problem, when:
//
// - a key is written twice in one file (JSON.parse keeps the last, so a
//   second use of a key silently replaces the first);
// - a key is in one catalogue and not the other (tsc also fails on a key
//   missing from ka.json, src/i18n/catalogues.ts);
// - a value is empty;
// - the {placeholders} of a key differ between en and ka;
// - any string, in either language, tells a pilot to manoeuvre: the USSP
//   informs and never resolves (CLAUDE.md rule 2, LESSONS X-15). The word
//   list is the brief's (climb, descend, hold, turn, avoid) in both
//   languages, matched as words and their inflections.
//
// `node scripts/check-i18n.mjs --self-test` runs the checks on built-in
// catalogues that break each rule (and one that breaks none) and fails
// unless every rule fires where it should and nowhere else (E-01).
import { readFileSync } from "node:fs";

/** English: the words and their inflections, as whole words (so "threshold" is not "hold"). */
const EN = [/\bclimb(s|ed|ing)?\b/i, /\bdescen(d|ds|ded|ding|t)\b/i, /\bhold(s|ing)?\b/i, /\bturn(s|ed|ing)?\b/i, /\bavoid(s|ed|ing|ance)?\b/i];

/**
 * Georgian: the stems of climb (ასვლა, აიწიე, აიმაღლე), descend
 * (დაშვება as a manoeuvre, დაეშვი, ჩაეშვი), hold (შეჩერება, გაჩერდი,
 * დაკიდება), turn (მოხვევა, მოუხვიე, შემობრუნება, შეტრიალება) and avoid
 * (აარიდე, თავიდან აცილება, აცდენა). The portal never uses დაშვება:
 * it would read as "descent".
 */
// Georgian has no : a stem that starts a word is one not preceded by a
// Georgian letter (so "ასვლა" is caught and "გასვლა", sign-out, is not).
const W = "(?:^|[^ა-ჿ])";
const KA = [new RegExp(`${W}ასვლ`), /აიწიე/, /აიმაღლ/, /დაშვებ/, /დაეშვ/, /ჩაეშვ/, /შეჩერ/, /გაჩერდ/, /დაკიდ/, /მოხვევ/, /მოუხვი/, /შემობრუნ/, /შეტრიალ/, /აარიდ/, /თავიდან აცილ/, /აცდენ/];

function placeholders(s) {
  return [...s.matchAll(/\{(\w+)\}/g)].map((m) => m[1]).sort().join(",");
}

/** The keys a catalogue's text writes more than once. */
export function duplicates(text) {
  const seen = new Set();
  const twice = new Set();
  for (const m of text.matchAll(/^\s*"((?:[^"\\]|\\.)*)"\s*:/gm)) {
    if (seen.has(m[1])) twice.add(m[1]);
    seen.add(m[1]);
  }
  return [...twice];
}

export function check(en, ka) {
  const problems = [];
  for (const k of Object.keys(en)) if (!(k in ka)) problems.push(`ka.json lacks ${k}`);
  for (const k of Object.keys(ka)) if (!(k in en)) problems.push(`ka.json has ${k}, which en.json does not`);
  for (const [lang, cat] of [["en", en], ["ka", ka]]) {
    for (const [k, v] of Object.entries(cat)) {
      if (typeof v !== "string" || v.trim() === "") problems.push(`${lang}.json ${k} is empty`);
    }
  }
  for (const k of Object.keys(en)) {
    if (k in ka && typeof en[k] === "string" && typeof ka[k] === "string" && placeholders(en[k]) !== placeholders(ka[k])) {
      problems.push(`${k}: placeholders {${placeholders(en[k])}} in en, {${placeholders(ka[k])}} in ka`);
    }
  }
  for (const [lang, cat, words] of [["en", en, EN], ["ka", ka, KA]]) {
    for (const [k, v] of Object.entries(cat)) {
      if (typeof v !== "string") continue;
      for (const w of words) if (w.test(v)) problems.push(`${lang}.json ${k} advises a manoeuvre (${w}): ${JSON.stringify(v)}`);
    }
  }
  return problems;
}

function selfTest() {
  let failedDup = 0;
  for (const [name, text, want] of [
    ["no duplicate", '{\n  "a": "x",\n  "b": "y"\n}', 0],
    ["a duplicate", '{\n  "a": "x",\n  "a": "y"\n}', 1],
  ]) {
    if (duplicates(text).length !== want) {
      console.error(`self-test ${name}: ${duplicates(text).length} duplicates, want ${want}`);
      failedDup++;
    }
  }
  const ok = { "a.b": "Threshold {n} m", "a.c": "Returned" };
  const okKa = { "a.b": "ზღვარი {n} მ", "a.c": "დაბრუნდა" };
  const cases = [
    ["clean", ok, okKa, 0],
    ["missing key", ok, { "a.b": "ზღვარი {n} მ" }, 1],
    ["extra key", ok, { ...okKa, "a.d": "x" }, 1],
    ["empty value", { ...ok, "a.c": " " }, okKa, 1],
    ["placeholders", ok, { ...okKa, "a.b": "ზღვარი {m} მ" }, 1],
    ["climb in en", { ...ok, "a.c": "Climb now" }, okKa, 1],
    ["hold in en", { ...ok, "a.c": "Holds the decision" }, okKa, 1],
    ["turn in en", { ...ok, "a.c": "Turn left" }, okKa, 1],
    ["avoid in en", { ...ok, "a.c": "Avoid the zone" }, okKa, 1],
    ["descend in en", { ...ok, "a.c": "Descend to 50 m" }, okKa, 1],
    ["descend in ka", ok, { ...okKa, "a.c": "დაეშვით 50 მ-მდე" }, 1],
    ["turn in ka", ok, { ...okKa, "a.c": "მოუხვიეთ მარცხნივ" }, 1],
    ["avoid in ka", ok, { ...okKa, "a.c": "აარიდეთ ზონას" }, 1],
    ["hold in ka", ok, { ...okKa, "a.c": "გაჩერდით" }, 1],
    ["climb in ka", ok, { ...okKa, "a.c": "აიწიეთ" }, 1],
    ["climb noun in ka", ok, { ...okKa, "a.c": "ასვლა 100 მ-მდე" }, 1],
    ["sign-out is not a climb", ok, { ...okKa, "a.c": "გასვლა" }, 0],
  ];
  let failed = 0;
  for (const [name, en, ka, want] of cases) {
    const got = check(en, ka).length;
    if (got !== want) {
      console.error(`self-test ${name}: ${got} problems, want ${want}`);
      failed++;
    }
  }
  if (failed + failedDup > 0) process.exit(1);
  console.log(`check-i18n self-test: ${cases.length} cases, each rule fires where it should and nowhere else`);
}

if (process.argv.includes("--self-test")) {
  selfTest();
} else {
  const text = (f) => readFileSync(new URL(`../src/i18n/${f}`, import.meta.url), "utf8");
  const en = JSON.parse(text("en.json"));
  const ka = JSON.parse(text("ka.json"));
  const problems = [
    ...duplicates(text("en.json")).map((k) => `en.json writes ${k} twice`),
    ...duplicates(text("ka.json")).map((k) => `ka.json writes ${k} twice`),
    ...check(en, ka),
  ];
  if (problems.length > 0) {
    for (const p of problems) console.error(p);
    console.error(`check-i18n: ${problems.length} problems`);
    process.exit(1);
  }
  console.log(`check-i18n: ${Object.keys(en).length} keys in en and ka, placeholders equal, no manoeuvre advice in either`);
}
