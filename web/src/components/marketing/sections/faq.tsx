import { Section, SectionHeading } from "@/components/marketing/primitives";
import {
  Accordion,
  AccordionContent,
  AccordionItem,
  AccordionTrigger,
} from "@/components/ui/accordion";
import { FAQ } from "@/components/marketing/content";

export function Faq() {
  return (
    <Section id="faq" variant="default">
      <SectionHeading eyebrow={FAQ.eyebrow} title={FAQ.title} align="center" />

      <Accordion className="mx-auto mt-12 max-w-3xl">
        {FAQ.items.map((item) => (
          <AccordionItem
            key={item.q}
            className="border-border not-last:border-b"
          >
            <AccordionTrigger className="gap-6 py-5 text-left text-base font-medium text-foreground no-underline hover:no-underline">
              {item.q}
            </AccordionTrigger>
            <AccordionContent className="max-w-2xl pb-5 text-base leading-relaxed text-muted-foreground">
              {item.a}
            </AccordionContent>
          </AccordionItem>
        ))}
      </Accordion>
    </Section>
  );
}
