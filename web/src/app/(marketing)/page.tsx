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

export default function HomePage() {
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
