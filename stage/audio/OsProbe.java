import java.lang.reflect.Method;

public class OsProbe {
    public static void main(String[] args) throws Exception {
        Class<?> os = Class.forName("android.system.Os");
        for (Method m : os.getDeclaredMethods()) {
            if (m.getName().toLowerCase().contains("socket")) {
                System.out.println(m);
            }
        }
        Class<?> c = Class.forName("android.system.OsConstants");
        System.out.println("AF_UNIX=" + c.getField("AF_UNIX").getInt(null) + " SOCK_SEQPACKET=" + c.getField("SOCK_SEQPACKET").getInt(null));
    }
}
