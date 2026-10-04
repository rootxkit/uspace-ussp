import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { ConsoleCasePage } from "@/console/EmergencyPages";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("console.title.case") };
}

/** One flight's emergency case (the flight id from the path; the API checks it). */
export default async function Page({ params }: { params: Promise<{ flight: string }> }) {
  const { flight } = await params;
  return <ConsoleCasePage flightId={flight} />;
}
