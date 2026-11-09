// Create asks the Host for a tier. The v2 contract has two interfaces that could be the Host; the plugin's typings
// were written for Users (plg types -host Users), so plg validate takes that one.
export async function Create(): Promise<rewards.Provider> {
  const tier = await host.Tier("admin");
  return { IsMember: async (user: string) => tier === "gold" };
}
