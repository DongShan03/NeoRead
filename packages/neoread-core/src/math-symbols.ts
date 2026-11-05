// Preserve Greek identity when moving between native PDF glyphs and LaTeX.
export const greekLatex: Record<string, string> = Object.fromEntries([
  ["α", "alpha"],
  ["β", "beta"],
  ["γ", "gamma"],
  ["δ", "delta"],
  ["ε", "varepsilon"],
  ["ϵ", "epsilon"],
  ["ζ", "zeta"],
  ["η", "eta"],
  ["θ", "theta"],
  ["ϑ", "vartheta"],
  ["ι", "iota"],
  ["κ", "kappa"],
  ["ϰ", "varkappa"],
  ["λ", "lambda"],
  ["μ", "mu"],
  ["ν", "nu"],
  ["ξ", "xi"],
  ["π", "pi"],
  ["ϖ", "varpi"],
  ["ρ", "rho"],
  ["ϱ", "varrho"],
  ["σ", "sigma"],
  ["ς", "varsigma"],
  ["τ", "tau"],
  ["υ", "upsilon"],
  ["φ", "varphi"],
  ["ϕ", "phi"],
  ["χ", "chi"],
  ["ψ", "psi"],
  ["ω", "omega"],
  ["Γ", "Gamma"],
  ["Δ", "Delta"],
  ["Θ", "Theta"],
  ["Λ", "Lambda"],
  ["Ξ", "Xi"],
  ["Π", "Pi"],
  ["Σ", "Sigma"],
  ["Υ", "Upsilon"],
  ["Φ", "Phi"],
  ["Ψ", "Psi"],
  ["Ω", "Omega"],
]);
const latexGreek = Object.fromEntries(
  Object.entries(greekLatex).map(([symbol, name]) => [name, symbol]),
);
export function latexGreekText(text: string) {
  // Unlike µ and Ω, the PDF compatibility glyph ∆ is unchanged by NFKD.
  // Normalize it only in the comparison text, retaining native PDF offsets.
  return text
    .replace(/∆/g, "Δ")
    .replace(/\\([A-Za-z]+)/g, (match, name: string) => latexGreek[name] ?? match);
}

// Mathematical Greek alphabets have the same 58 slots, including the variants.
const greekAlphabet = Array.from(
  "ΑΒΓΔΕΖΗΘΙΚΛΜΝΞΟΠΡϴΣΤΥΦΧΨΩ∇αβγδεζηθικλμνξοπρςστυφχψω∂ϵϑϰϕϱϖ",
);
export function mathGlyphLatex(letter: string) {
  const code = letter.codePointAt(0)!;
  for (const [base, bold] of [
    [0x1d6a8, true],
    [0x1d6e2, false],
    [0x1d71c, true],
    [0x1d756, true],
    [0x1d790, true],
  ] as const) {
    if (code < base || code >= base + greekAlphabet.length) continue;
    const symbol = greekAlphabet[code - base];
    const atom = greekLatex[symbol]
      ? `\\${greekLatex[symbol]}`
      : symbol === "∇"
        ? "\\nabla"
        : symbol === "∂"
          ? "\\partial"
          : symbol;
    return bold ? `\\mathbf{${atom}}` : atom + " ";
  }
  if (greekLatex[letter]) return `\\${greekLatex[letter]} `;
  if (/[𝐀-𝐳]/u.test(letter)) return `\\mathbf{${letter.normalize("NFKD")}}`;
  if (/[𝒜-𝒵ℬℰℱℋℐℒℳℛ]/u.test(letter))
    return `\\mathcal{${letter.normalize("NFKD")}}`;
  if (/[𝔸-𝕑ℂℍℕℙℚℝℤ]/u.test(letter))
    return `\\mathbb{${letter.normalize("NFKD")}}`;
  if (/[𝑨-𝒛]/u.test(letter)) return `\\mathbf{${letter.normalize("NFKD")}}`;
  if (/[𝐴-𝑧ℎ]/u.test(letter)) return letter.normalize("NFKD");
  return letter;
}
