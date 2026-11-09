// An eligibility condition written in TypeScript: users in the United States or Mexico qualify, the marketplace's
// North American campaigns. Its types are plg.d.ts's (plg types writes them).
const countries = ["US", "MX"];

class Condition implements eligibility.Condition {
  Eligible(subject: eligibility.Subject): eligibility.Decision {
    const country = subject.user.country;
    if (countries.includes(country)) {
      return { eligible: true, reason: `lives in ${country}` };
    }
    return { eligible: false, reason: `lives in ${country}, outside North America` };
  }

  // Validate rejects the condition when it targets a country the platform does not serve.
  Validate(limits: eligibility.Limits): plg.Issue[] {
    const served = new Set(limits.countries ?? []);
    return countries.filter((country) => !served.has(country)).map((country) => ({
      severity: "error",
      code: "north-america.country",
      message: `targets ${country}, which the platform does not serve`,
      field: "countries",
    }));
  }
}

export function Create(): eligibility.Condition {
  return new Condition();
}
