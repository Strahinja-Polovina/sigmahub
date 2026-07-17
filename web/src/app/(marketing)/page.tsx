import { headers } from "next/headers";
import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { Hero } from "@/components/marketing/sections/hero";
import { WorksWith } from "@/components/marketing/sections/works-with";
import { Problem } from "@/components/marketing/sections/problem";
import { Pillars } from "@/components/marketing/sections/pillars";
import { Architecture } from "@/components/marketing/sections/architecture";
import { Compare } from "@/components/marketing/sections/compare";
import { Cost } from "@/components/marketing/sections/cost";
import { UseCases } from "@/components/marketing/sections/use-cases";
import { Onboarding } from "@/components/marketing/sections/onboarding";
import { Pricing } from "@/components/marketing/sections/pricing";
import { Faq } from "@/components/marketing/sections/faq";
import { ClosingCta } from "@/components/marketing/sections/cta";

export default async function HomePage() {
  // A signed-in user landing on the marketing root belongs in the dashboard —
  // without this, logging in and later opening the site root strands them on
  // the marketing page with no path forward.
  const session = await auth.api.getSession({ headers: await headers() });
  if (session) redirect("/dashboard");
  return (
    <>
      <Hero />
      <WorksWith />
      <Problem />
      <Pillars />
      <Architecture />
      <Compare />
      <Cost />
      <UseCases />
      <Onboarding />
      <Pricing />
      <Faq />
      <ClosingCta />
    </>
  );
}
