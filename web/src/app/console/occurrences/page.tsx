import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { ConsoleOccurrencesPage } from "@/console/RecordsPages";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("console.title.occurrences") };
}

export default function Page() {
  return <ConsoleOccurrencesPage />;
}
