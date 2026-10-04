import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { ConsoleFlightsPage } from "@/console/FlightsPage";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("console.title.flights") };
}

export default function Page() {
  return <ConsoleFlightsPage />;
}
