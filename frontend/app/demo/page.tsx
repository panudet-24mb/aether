import Link from "next/link";
import Demo from "../demo-page";
export default function Page() { return <><div style={{ padding: 12, background: '#44282d', color: '#ffced0', textAlign: 'center' }}>ข้อมูลจำลอง — <Link href="/" style={{ textDecoration: 'underline' }}>กลับไปข้อมูลจริง</Link></div><Demo /></>; }
